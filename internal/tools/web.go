package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/zendev-sh/goai"
)

const (
	webFetchTimeout = 30 * time.Second
	webFetchMaxBody = 1024 * 1024 // 1MB
)

type webFetchInput struct {
	URL string `json:"url" jsonschema:"description=要抓取的 URL（http/https）"`
}

func webFetchTool() Tool {
	client := &http.Client{Timeout: webFetchTimeout}
	return Tool{
		GoAITool: goai.NewTool("web_fetch", "抓取网页内容并转为纯文本。用于查阅文档、搜索资料。HTML 页面会去除标签与脚本，返回可读文本。",
			func(ctx context.Context, in webFetchInput) (string, error) {
				if in.URL == "" {
					return "", fmt.Errorf("url 不能为空")
				}
				if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
					return "", fmt.Errorf("仅支持 http/https URL: %q", in.URL)
				}

				req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
				if err != nil {
					return "", err
				}
				req.Header.Set("User-Agent", "zlite/0.1 (+https://github.com/helloxz/zlite)")

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
					text = htmlToText(body)
				}
				if truncated {
					text += "\n...[响应超过 1MB 已截断]"
				}
				return text, nil
			}),
		Modes: []Mode{ModePlan, ModeBuild},
	}
}

var (
	scriptRe      = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	styleRe       = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	commentRe     = regexp.MustCompile(`(?s)<!--.*?-->`)
	tagRe         = regexp.MustCompile(`(?s)<[^>]+>`)
	inlineSpaceRe = regexp.MustCompile(`[ \t\r]+`)
	blankLineRe   = regexp.MustCompile(`\n{3,}`)
)

// htmlToText 去除 HTML 标签与脚本，返回可读纯文本（不引入第三方库）。
func htmlToText(b []byte) string {
	s := string(b)
	s = scriptRe.ReplaceAllString(s, " ")
	s = styleRe.ReplaceAllString(s, " ")
	s = commentRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = inlineSpaceRe.ReplaceAllString(s, " ")
	s = blankLineRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
