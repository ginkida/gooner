package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gokin/internal/security"

	"golang.org/x/net/html"
	"google.golang.org/genai"
)

// WebFetchTool fetches content from URLs and converts HTML to markdown.
type WebFetchTool struct {
	client  *http.Client
	maxSize int64
}

// NewWebFetchTool creates a new web fetch tool.
func NewWebFetchTool() *WebFetchTool {
	// Create secure HTTP client with TLS 1.2+ enforcement
	secureClient, err := security.CreateDefaultHTTPClient()
	if err != nil {
		// Fall back to default client if secure client creation fails
		// This should never happen with default config
		secureClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}

	return &WebFetchTool{
		client:  secureClient,
		maxSize: 1024 * 1024, // 1MB max
	}
}

func (t *WebFetchTool) Name() string {
	return "web_fetch"
}

func (t *WebFetchTool) Description() string {
	return "Fetches content from a URL and returns it as markdown. Useful for reading documentation, articles, or any web content."
}

func (t *WebFetchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The URL to fetch content from",
				},
				"selector": {
					Type:        genai.TypeString,
					Description: "Optional CSS-like selector to extract specific content (e.g., 'article', 'main', '.content')",
				},
			},
			Required: []string{"url"},
		},
	}
}

func (t *WebFetchTool) Validate(args map[string]any) error {
	urlStr, ok := GetString(args, "url")
	if !ok || urlStr == "" {
		return NewValidationError("url", "is required")
	}

	// Validate URL format
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return NewValidationError("url", fmt.Sprintf("invalid URL: %s", err))
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return NewValidationError("url", "only http and https URLs are supported")
	}

	// SSRF protection: validate URL does not target internal/private networks
	result := security.ValidateURLForSSRF(urlStr)
	if !result.Valid {
		return NewValidationError("url", fmt.Sprintf("SSRF protection: %s", result.Reason))
	}

	return nil
}

func (t *WebFetchTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	urlStr, _ := GetString(args, "url")
	selector, _ := GetString(args, "selector")

	// Create request
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to create request: %s", err)), nil
	}

	// Set user agent
	req.Header.Set("User-Agent", "Gokin/1.0 (AI Assistant)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	// Fetch
	resp, err := t.client.Do(req)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to fetch URL: %s", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return NewErrorResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status)), nil
	}

	// Read body with size limit
	limitedReader := io.LimitReader(resp.Body, t.maxSize)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to read response: %s", err)), nil
	}

	// Check content type
	contentType := resp.Header.Get("Content-Type")

	var content string
	if strings.Contains(strings.ToLower(contentType), "text/html") || strings.Contains(strings.ToLower(contentType), "application/xhtml") {
		// Parse and convert HTML to markdown
		content, err = t.htmlToMarkdown(string(body), selector)
		if err != nil {
			return NewErrorResult(fmt.Sprintf("failed to parse HTML: %s", err)), nil
		}
	} else if strings.Contains(contentType, "text/plain") || strings.Contains(contentType, "application/json") {
		content = string(body)
	} else {
		// Try to extract text anyway
		content, _ = t.htmlToMarkdown(string(body), selector)
		if content == "" {
			content = string(body)
		}
	}

	// Truncate if too long
	const maxLen = 50000
	if len(content) > maxLen {
		content = content[:maxLen] + "\n\n... (content truncated)"
	}

	return NewSuccessResultWithData(content, map[string]any{
		"url":          urlStr,
		"status":       resp.StatusCode,
		"content_type": contentType,
		"length":       len(content),
	}), nil
}

// htmlToMarkdown converts HTML to markdown-like text.
func (t *WebFetchTool) htmlToMarkdown(htmlContent string, selector string) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "", err
	}

	var content strings.Builder
	var extract func(*html.Node, int)

	// Skip these tags
	skipTags := map[string]bool{
		"script": true, "style": true, "nav": true, "footer": true,
		"header": true, "aside": true, "noscript": true, "iframe": true,
	}

	// Block elements that need newlines
	blockTags := map[string]bool{
		"p": true, "div": true, "section": true, "article": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"li": true, "tr": true, "br": true, "hr": true,
		"blockquote": true, "pre": true, "table": true,
	}

	extract = func(n *html.Node, depth int) {
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)

			// Skip unwanted tags
			if skipTags[tag] {
				return
			}

			// Handle specific tags
			switch tag {
			case "h1":
				content.WriteString("\n# ")
			case "h2":
				content.WriteString("\n## ")
			case "h3":
				content.WriteString("\n### ")
			case "h4":
				content.WriteString("\n#### ")
			case "h5":
				content.WriteString("\n##### ")
			case "h6":
				content.WriteString("\n###### ")
			case "li":
				content.WriteString("\n- ")
			case "br":
				content.WriteString("\n")
			case "hr":
				content.WriteString("\n---\n")
			case "code":
				content.WriteString("`")
			case "pre":
				content.WriteString("\n```\n")
			case "strong", "b":
				content.WriteString("**")
			case "em", "i":
				content.WriteString("*")
			case "a":
				// Will handle href after children
			case "p", "div", "section", "article", "blockquote":
				content.WriteString("\n")
			}
		}

		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				// Normalize whitespace
				text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
				content.WriteString(text)
			}
		}

		// Process children
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			extract(c, depth+1)
		}

		// Close tags
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			switch tag {
			case "code":
				content.WriteString("`")
			case "pre":
				content.WriteString("\n```\n")
			case "strong", "b":
				content.WriteString("**")
			case "em", "i":
				content.WriteString("*")
			case "a":
				// Add link
				for _, attr := range n.Attr {
					if attr.Key == "href" && attr.Val != "" && !strings.HasPrefix(attr.Val, "#") && !strings.HasPrefix(attr.Val, "javascript:") {
						content.WriteString(fmt.Sprintf(" (%s)", attr.Val))
						break
					}
				}
			}

			if blockTags[tag] {
				content.WriteString("\n")
			}
		}
	}

	// Find body or use whole document
	var startNode *html.Node
	var findBody func(*html.Node) *html.Node
	findBody = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "body" {
				return n
			}
			// If selector specified, try to match
			if selector != "" {
				if t.matchesSelector(n, selector) {
					return n
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if found := findBody(c); found != nil {
				return found
			}
		}
		return nil
	}

	startNode = findBody(doc)
	if startNode == nil {
		startNode = doc
	}

	extract(startNode, 0)

	// Clean up result
	result := content.String()
	result = regexp.MustCompile(`\n{3,}`).ReplaceAllString(result, "\n\n")
	result = strings.TrimSpace(result)

	return result, nil
}

// matchesSelector checks if a node matches a simple CSS selector.
func (t *WebFetchTool) matchesSelector(n *html.Node, selector string) bool {
	selector = strings.TrimSpace(selector)

	// Class selector
	if strings.HasPrefix(selector, ".") {
		className := selector[1:]
		for _, attr := range n.Attr {
			if attr.Key == "class" {
				classes := strings.Fields(attr.Val)
				for _, c := range classes {
					if c == className {
						return true
					}
				}
			}
		}
		return false
	}

	// ID selector
	if strings.HasPrefix(selector, "#") {
		id := selector[1:]
		for _, attr := range n.Attr {
			if attr.Key == "id" && attr.Val == id {
				return true
			}
		}
		return false
	}

	// Tag selector
	return strings.ToLower(n.Data) == strings.ToLower(selector)
}
