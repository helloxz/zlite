package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zendev-sh/goai"
)

const (
	// webSearchTimeout 是单次 Tavily 搜索请求的超时时间（用户要求 30s）。
	webSearchTimeout = 30 * time.Second
	// webSearchMaxKeywords 是单次调用允许的最大关键词数量（用户要求最多 3 个）。
	webSearchMaxKeywords = 3
	// webSearchMaxResults 是每次搜索返回的结果条数（用户要求固定 3）。
	webSearchMaxResults = 3
	// webSearchSnippetLen 是单条结果 content 保留的最大长度（rune），防止输出过大撑爆上下文。
	webSearchSnippetLen = 200
)

// webSearchAPI 与 webSearchInterval 用 var 声明，便于测试替换（httptest server、0 间隔），生产行为不变。
var (
	// webSearchAPI 是 Tavily 搜索接口地址。
	webSearchAPI = "https://api.tavily.com/search"
	// webSearchInterval 是相邻两次搜索请求之间的固定间隔（用户要求 5s）。
	webSearchInterval = 5 * time.Second
)

// webSearchInput 是 web_search 工具的输入：由模型从用户需求中提取 1-3 个关键词。
type webSearchInput struct {
	Keywords []string `json:"keywords" jsonschema:"description=搜索关键词（1-3 个），从用户的需求中提取；最多 3 个"`
}

// tavilyResult 是 Tavily 返回的单条搜索结果。
type tavilyResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// tavilyResponse 是 Tavily /search 接口的响应体（只解析用到的字段）。
type tavilyResponse struct {
	Results []tavilyResult `json:"results"`
}

// webSearchTool 构造 web_search 工具：对每个关键词分别调用 Tavily 搜索并汇总结果。
// 触发条件由工具描述约束：仅当用户明确要求联网搜索时才使用。
func webSearchTool() Tool {
	client := &http.Client{Timeout: webSearchTimeout}
	return Tool{
		GoAITool: goai.NewTool("web_search",
			"联网搜索（Tavily）。仅当用户明确要求搜索网络、查询最新信息或给出可搜索的内容时才调用；用户意图不明确时不要调用，应直接询问用户。"+
				"从用户的需求中提取 1-3 个搜索关键词（最多 3 个），例如用户问“grok 4.6 跑分”可提取 [\"grok 4.6 benchmark\", \"grok 4.6 跑分\"]。"+
				"工具会对每个关键词分别搜索并汇总结果；如需抓取搜索结果中的具体网页，请改用 web_fetch。",
			func(ctx context.Context, in webSearchInput) (string, error) {
				// 防御性校验：关键词数量约束是模型行为层面的软约束，这里做硬校验兜底。
				if len(in.Keywords) == 0 {
					return "", fmt.Errorf("keywords 不能为空：请从用户需求中提取 1-3 个搜索关键词")
				}
				if len(in.Keywords) > webSearchMaxKeywords {
					in.Keywords = in.Keywords[:webSearchMaxKeywords]
				}

				var out strings.Builder
				var failCount int
				for i, kw := range in.Keywords {
					// 相邻请求之间固定间隔 5s；用 select 响应 ctx 取消，避免用户中断时卡死。
					if i > 0 {
						select {
						case <-ctx.Done():
							return "", ctx.Err()
						case <-time.After(webSearchInterval):
						}
					}

					results, err := tavilySearch(ctx, client, kw)
					if err != nil {
						// 单个关键词失败（限流/网络/超时等）时跳过并记录原因，继续其余关键词。
						failCount++
						out.WriteString(fmt.Sprintf("## 关键词 %q：搜索失败（%v）\n\n", kw, err))
						continue
					}
					writeTavilyResults(&out, kw, results)
				}

				// 全部关键词都失败才整体报错；部分失败时把失败信息一并交给 AI 判断。
				if failCount == len(in.Keywords) {
					return "", fmt.Errorf("全部 %d 个关键词搜索失败", failCount)
				}
				return out.String(), nil
			}),
		Modes: []Mode{ModePlan, ModeBuild},
	}
}

// tavilySearch 调用 Tavily /search 接口（keyless 模式），返回该关键词的结果列表。
func tavilySearch(ctx context.Context, client *http.Client, keyword string) ([]tavilyResult, error) {
	payload, err := json.Marshal(map[string]any{
		"query":       keyword,
		"max_results": webSearchMaxResults,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webSearchAPI, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// keyless 模式：无需 API key，但有严格速率限制，失败由调用方跳过处理。
	req.Header.Set("X-Tavily-Access-Mode", "keyless")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 错误响应体一般很小，限制读取长度防止异常响应撑爆内存。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	var tr tavilyResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return tr.Results, nil
}

// writeTavilyResults 把单个关键词的搜索结果整理为文本块写入 out。
// 每条结果只保留 title、url 和截断后的 content，控制输出体积。
func writeTavilyResults(out *strings.Builder, keyword string, results []tavilyResult) {
	out.WriteString(fmt.Sprintf("## 关键词 %q 的搜索结果（%d 条）\n\n", keyword, len(results)))
	for idx, r := range results {
		out.WriteString(fmt.Sprintf("%d. **%s**\n   URL: %s\n   %s\n\n",
			idx+1, r.Title, r.URL, clipText(r.Content, webSearchSnippetLen)))
	}
}

// clipText 把文本压平为单行并截断到最多 n 个字符（按 rune 计，中文不截半），超长加省略号。
func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
