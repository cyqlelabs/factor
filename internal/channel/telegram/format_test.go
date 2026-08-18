package telegram

import "testing"

func TestFormatHTML(t *testing.T) {
	tests := map[string]struct {
		md   string
		want string
	}{
		"plain text passes through": {"hello world", "hello world"},
		"soft line break":           {"one\ntwo", "one\ntwo"},
		"hard line break":           {"one  \ntwo", "one\ntwo"},
		"paragraphs":                {"one\n\n\n\ntwo", "one\n\ntwo"},
		"bold":                      {"a **bold** move", "a <b>bold</b> move"},
		"italic":                    {"lean *in*", "lean <i>in</i>"},
		"bold italic":               {"***both***", "<i><b>both</b></i>"},
		"strikethrough":             {"~~gone~~", "<s>gone</s>"},
		"heading":                   {"# Title\n\nbody", "<b>Title</b>\n\nbody"},
		"heading with code":         {"## Use `go vet`", "<b>Use <code>go vet</code></b>"},
		"code span":                 {"run `make check` now", "run <code>make check</code> now"},
		"code span escapes":         {"`a < b`", "<code>a &lt; b</code>"},
		"fenced code with language": {"```go\nif a < b {\n}\n```", "<pre><code class=\"language-go\">if a &lt; b {\n}</code></pre>"},
		"fenced code plain":         {"```\nplain\n```", "<pre>plain</pre>"},
		"unclosed fence":            {"```\ndangling", "<pre>dangling</pre>"},
		"indented code":             {"    tabbed", "<pre>tabbed</pre>"},
		"bullet list":               {"- one\n- two", "• one\n• two"},
		"nested list":               {"- a\n  - b\n- c", "• a\n  • b\n• c"},
		"loose list":                {"- a\n\n- b", "• a\n• b"},
		"ordered list":              {"1. one\n2. two", "1. one\n2. two"},
		"ordered list start":        {"3. three\n4. four", "3. three\n4. four"},
		"list then paragraph":       {"- a\n\ntail", "• a\n\ntail"},
		"blockquote":                {"> wise words", "<blockquote>wise words</blockquote>"},
		"blockquote paragraphs":     {"> a\n>\n> b", "<blockquote>a\n\nb</blockquote>"},
		"link":                      {"[docs](https://x.dev)", `<a href="https://x.dev">docs</a>`},
		"link href escapes":         {"[q](https://x.dev/?a=1&b=2)", `<a href="https://x.dev/?a=1&amp;b=2">q</a>`},
		"image links its alt":       {"![diagram](https://x.dev/d.png)", `<a href="https://x.dev/d.png">diagram</a>`},
		"image without alt":         {"![](https://x.dev/d.png)", `<a href="https://x.dev/d.png">https://x.dev/d.png</a>`},
		"autolink stays bare":       {"<https://x.dev>", "https://x.dev"},
		"specials escaped":          {"a < b && c > d", "a &lt; b &amp;&amp; c &gt; d"},
		"inline html escaped":       {"a <b>tag</b>", "a &lt;b&gt;tag&lt;/b&gt;"},
		"html block escaped":        {"<div>\nboo\n</div>", "&lt;div&gt;\nboo\n&lt;/div&gt;"},
		"thematic break":            {"above\n\n---\n\nbelow", "above\n\n———\n\nbelow"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := formatHTML(tt.md); got != tt.want {
				t.Errorf("formatHTML(%q) = %q, want %q", tt.md, got, tt.want)
			}
		})
	}
}
