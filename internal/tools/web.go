package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) factor-agent/1.0"

// NewWebTools returns web_fetch and web_search (DuckDuckGo HTML, no API key).
func NewWebTools() []Tool {
	client := &http.Client{Timeout: 30 * time.Second}
	return []Tool{
		&webFetchTool{client: client},
		&webSearchTool{client: client, searchURL: "https://html.duckduckgo.com/html/"},
	}
}

type webFetchTool struct{ client *http.Client }

func (t *webFetchTool) Name() string { return "web_fetch" }
func (t *webFetchTool) Description() string {
	return "Fetch a URL and return its readable text content (HTML is converted to text). It does not run JavaScript, so a modern site often answers with an empty shell: when the text comes back thin, blocked, or missing what was asked for, open the page with browser_navigate instead of reporting what the shell said."
}
func (t *webFetchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":       map[string]any{"type": "string", "description": "Absolute http:// or https:// URL"},
			"max_chars": map[string]any{"type": "integer", "description": "Cap on returned characters (default 20000)"},
		},
		"required": []any{"url"},
	}
}

func (t *webFetchTool) Execute(ctx context.Context, args map[string]any) *Result {
	rawURL := StringArg(args, "url")
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return Errorf("invalid url: %q (must be http/https)", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Errorf("%v", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return Errorf("fetch failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Errorf("read failed: %v", err)
	}
	if resp.StatusCode >= 400 {
		return Errorf("HTTP %d from %s", resp.StatusCode, u.Host)
	}

	maxChars := IntArg(args, "max_chars", 20000)
	content := string(body)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") || looksLikeHTML(content) {
		title, text := extractReadableText(content)
		content = text
		if title != "" {
			content = title + "\n\n" + text
		}
	}
	if len(content) > maxChars {
		content = content[:maxChars] + "\n... [truncated]"
	}
	return Text(content)
}

func looksLikeHTML(s string) bool {
	head := strings.ToLower(s[:min(len(s), 512)])
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

// extractReadableText strips scripts/styles/nav and collapses whitespace.
func extractReadableText(src string) (title, text string) {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return "", src
	}
	var b strings.Builder
	skip := map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "iframe": true}
	var walk func(*html.Node)
	var inTitle bool
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skip[n.Data] {
				return
			}
			if n.Data == "title" {
				inTitle = true
				defer func() { inTitle = false }()
			}
		}
		if n.Type == html.TextNode {
			trimmed := strings.TrimSpace(n.Data)
			if trimmed != "" {
				if inTitle && title == "" {
					title = trimmed
				} else if !inTitle {
					b.WriteString(trimmed)
					b.WriteByte('\n')
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, strings.TrimSpace(b.String())
}

type webSearchTool struct {
	client    *http.Client
	searchURL string
}

func (t *webSearchTool) Name() string { return "web_search" }
func (t *webSearchTool) Description() string {
	return "Search the web (DuckDuckGo). Returns titles, URLs, and snippets. Snippets are a starting point, not an answer: open the pages that matter."
}
func (t *webSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Search keywords, as you would type them into a search box"},
			"count": map[string]any{"type": "integer", "description": "Results to return (default 5, max 10)"},
		},
		"required": []any{"query"},
	}
}

type searchResult struct{ title, href, snippet string }

func (t *webSearchTool) Execute(ctx context.Context, args map[string]any) *Result {
	query := StringArg(args, "query")
	count := min(max(IntArg(args, "count", 5), 1), 10)

	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.searchURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Errorf("%v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return Errorf("search failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Errorf("read failed: %v", err)
	}
	if resp.StatusCode != 200 {
		return Errorf("search returned HTTP %d", resp.StatusCode)
	}

	results := parseDuckDuckGo(string(body))
	if len(results) == 0 {
		return Text("No results found.")
	}
	if len(results) > count {
		results = results[:count]
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, r.title, r.href, r.snippet)
	}
	return Text(b.String())
}

// parseDuckDuckGo extracts results from the html.duckduckgo.com layout.
func parseDuckDuckGo(src string) []searchResult {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return nil
	}
	var results []searchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			class := attr(n, "class")
			switch {
			case strings.Contains(class, "result__a"):
				results = append(results, searchResult{
					title: nodeText(n),
					href:  cleanDDGHref(attr(n, "href")),
				})
			case strings.Contains(class, "result__snippet"):
				if len(results) > 0 && results[len(results)-1].snippet == "" {
					results[len(results)-1].snippet = nodeText(n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return results
}

// cleanDDGHref decodes DuckDuckGo's redirect links (//duckduckgo.com/l/?uddg=...).
func cleanDDGHref(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if strings.Contains(u.Host, "duckduckgo.com") {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
	}
	return href
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}
