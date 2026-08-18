// format.go renders the Markdown the model writes into the HTML subset
// Telegram accepts (https://core.telegram.org/bots/api#html-style). Telegram
// has no headings, lists or rules, so those become bold lines, bullet
// prefixes and a dash line; everything else maps to its tag, and HTML in the
// source is escaped so it shows as text instead of corrupting the parse.
package telegram

import (
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var mdParser = goldmark.New(goldmark.WithExtensions(extension.Strikethrough)).Parser()

var (
	textEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attrEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

// formatHTML converts one outbound Markdown message to Telegram HTML.
func formatHTML(md string) string {
	src := []byte(md)
	w := &htmlWriter{src: src, ordinals: map[ast.Node]int{}}
	_ = ast.Walk(mdParser.Parse(text.NewReader(src)), w.visit)
	return strings.TrimSpace(string(w.buf))
}

type htmlWriter struct {
	src      []byte
	buf      []byte
	ordinals map[ast.Node]int // items numbered so far, per ordered list
}

func (w *htmlWriter) text(s string)    { w.buf = append(w.buf, s...) }
func (w *htmlWriter) escaped(b []byte) { w.buf = append(w.buf, textEsc.Replace(string(b))...) }

// endBlock separates a finished block element from whatever follows it by
// exactly one blank line.
func (w *htmlWriter) endBlock() {
	w.trimNewlines()
	w.text("\n\n")
}

func (w *htmlWriter) trimNewlines() {
	for len(w.buf) > 0 && w.buf[len(w.buf)-1] == '\n' {
		w.buf = w.buf[:len(w.buf)-1]
	}
}

// lines writes a block node's raw source lines, escaped.
func (w *htmlWriter) lines(n ast.Node) {
	l := n.Lines()
	for i := 0; i < l.Len(); i++ {
		seg := l.At(i)
		w.escaped(seg.Value(w.src))
	}
}

// codeBlock emits a fenced or indented block as <pre>, keeping the fence's
// language as the class Telegram uses for syntax highlighting.
func (w *htmlWriter) codeBlock(n ast.Node, lang string) {
	if lang != "" {
		w.text(`<pre><code class="language-` + attrEsc.Replace(lang) + `">`)
	} else {
		w.text("<pre>")
	}
	w.lines(n)
	w.trimNewlines()
	if lang != "" {
		w.text("</code></pre>")
	} else {
		w.text("</pre>")
	}
	w.endBlock()
}

// listDepth counts the lists above this item's own, for indentation.
func listDepth(item ast.Node) int {
	depth := -1
	for p := item.Parent(); p != nil; p = p.Parent() {
		if _, ok := p.(*ast.List); ok {
			depth++
		}
	}
	return depth
}

func (w *htmlWriter) visit(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch n := n.(type) {
	case *ast.Heading:
		if entering {
			w.text("<b>")
		} else {
			w.text("</b>")
			w.endBlock()
		}
	case *ast.Paragraph:
		if !entering {
			if _, inItem := n.Parent().(*ast.ListItem); inItem {
				w.trimNewlines()
				w.text("\n")
			} else {
				w.endBlock()
			}
		}
	case *ast.TextBlock:
		if !entering {
			w.text("\n")
		}
	case *ast.Blockquote:
		if entering {
			w.text("<blockquote>")
		} else {
			w.trimNewlines()
			w.text("</blockquote>")
			w.endBlock()
		}
	case *ast.List:
		if !entering {
			if _, nested := n.Parent().(*ast.ListItem); nested {
				w.trimNewlines()
				w.text("\n")
			} else {
				w.endBlock()
			}
		}
	case *ast.ListItem:
		if entering {
			w.text(strings.Repeat("  ", listDepth(n)))
			list := n.Parent().(*ast.List)
			if list.IsOrdered() {
				w.text(strconv.Itoa(list.Start+w.ordinals[list]) + ". ")
				w.ordinals[list]++
			} else {
				w.text("• ")
			}
		} else if len(w.buf) > 0 && w.buf[len(w.buf)-1] != '\n' {
			w.text("\n")
		}
	case *ast.FencedCodeBlock:
		if entering {
			w.codeBlock(n, string(n.Language(w.src)))
		}
		return ast.WalkSkipChildren, nil
	case *ast.CodeBlock:
		if entering {
			w.codeBlock(n, "")
		}
		return ast.WalkSkipChildren, nil
	case *ast.CodeSpan:
		if entering {
			w.text("<code>")
		} else {
			w.text("</code>")
		}
	case *ast.Emphasis:
		tag := "i"
		if n.Level >= 2 {
			tag = "b"
		}
		if entering {
			w.text("<" + tag + ">")
		} else {
			w.text("</" + tag + ">")
		}
	case *east.Strikethrough:
		if entering {
			w.text("<s>")
		} else {
			w.text("</s>")
		}
	case *ast.Link:
		if entering {
			w.text(`<a href="` + attrEsc.Replace(string(n.Destination)) + `">`)
		} else {
			w.text("</a>")
		}
	case *ast.Image:
		// A chat can't inline an image by URL, so link its alt text — or the
		// URL itself, which an empty alt would otherwise leave invisible.
		if entering {
			w.text(`<a href="` + attrEsc.Replace(string(n.Destination)) + `">`)
			if !n.HasChildren() {
				w.escaped(n.Destination)
			}
		} else {
			w.text("</a>")
		}
	case *ast.AutoLink:
		if entering {
			w.escaped(n.Label(w.src)) // bare URLs Telegram links on its own
		}
	case *ast.Text:
		if entering {
			w.escaped(n.Segment.Value(w.src))
			if n.SoftLineBreak() || n.HardLineBreak() {
				w.text("\n")
			}
		}
	case *ast.String:
		if entering {
			w.escaped(n.Value)
		}
	case *ast.ThematicBreak:
		if entering {
			w.text("———")
			w.endBlock()
		}
	case *ast.HTMLBlock:
		if entering {
			w.lines(n)
			if n.HasClosure() {
				w.escaped(n.ClosureLine.Value(w.src))
			}
			w.endBlock()
		}
		return ast.WalkSkipChildren, nil
	case *ast.RawHTML:
		if entering {
			for i := 0; i < n.Segments.Len(); i++ {
				seg := n.Segments.At(i)
				w.escaped(seg.Value(w.src))
			}
		}
		return ast.WalkSkipChildren, nil
	}
	return ast.WalkContinue, nil
}
