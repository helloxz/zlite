package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	htmlx "github.com/cybergodev/html"
	"github.com/zendev-sh/goai"
)

const (
	webFetchTimeout = 60 * time.Second
	webFetchMaxBody = 1024 * 1024 // 1MB

	// chromeUA 使用真实 Chrome UA，避免被目标站点识别为爬虫而拒绝访问
	chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

type webFetchInput struct {
	URL string `json:"url" jsonschema:"description=要抓取的 URL（http/https）"`
}

func webFetchTool() Tool {
	client := &http.Client{Timeout: webFetchTimeout}
	return Tool{
		GoAITool: goai.NewTool("web_fetch", "抓取指定 URL 的网页内容并转为纯文本。用于查阅文档、访问已知链接（如 web_search 返回的结果）。HTML 页面会去除标签与脚本，返回可读文本。",
			func(ctx context.Context, in webFetchInput) (string, error) {
				if in.URL == "" {
					return "", fmt.Errorf("url 不能为空")
				}
				if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
					return "", fmt.Errorf("仅支持 http/https URL: %q", in.URL)
				}

				u, err := url.Parse(in.URL)
				if err != nil {
					return "", err
				}

				req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
				if err != nil {
					return "", err
				}
				req.Header.Set("User-Agent", chromeUA)
				// 伪装 Referer 为目标站点首页（scheme://host，去掉路径），使请求看起来像站内跳转
				req.Header.Set("Referer", u.Scheme+"://"+u.Host)

				resp, err := client.Do(req)
				if err != nil {
					return "", err
				}
				defer resp.Body.Close()

				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
				}

				body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBody+1))
				if err != nil {
					return "", err
				}
				truncated := len(body) > webFetchMaxBody
				if truncated {
					body = body[:webFetchMaxBody]
				}

				ct := resp.Header.Get("Content-Type")
				text := string(body)
				if strings.Contains(ct, "html") {
					// 使用 cybergodev/html 库清洗 HTML 并转换为 Markdown，方便 AI 处理
					md, err := htmlx.ExtractToMarkdownWithContext(ctx, body, htmlx.MarkdownConfig())
					if err != nil {
						return "", fmt.Errorf("HTML 清洗失败: %w", err)
					}
					text = md
				}
				if truncated {
					text += "\n...[响应超过 1MB 已截断]"
				}
				return text, nil
			}),
		Modes: []Mode{ModePlan, ModeBuild},
	}
}
