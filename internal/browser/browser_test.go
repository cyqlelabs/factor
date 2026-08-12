//go:build !nobrowser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

func TestSelectorForRefMapping(t *testing.T) {
	s := NewSession(config.BrowserConfig{}, t.TempDir())
	s.refs["e1"] = "div > button:nth-child(2)"
	if got := s.selectorFor("e1"); got != "div > button:nth-child(2)" {
		t.Errorf("ref lookup = %q", got)
	}
	if got := s.selectorFor("#raw"); got != "#raw" {
		t.Errorf("raw selector = %q", got)
	}
}

func TestFormatRead(t *testing.T) {
	r := &pageRead{Title: "T", URL: "http://x", Text: "body text"}
	r.Elements = append(r.Elements, struct {
		Ref      string `json:"ref"`
		Tag      string `json:"tag"`
		Type     string `json:"type"`
		Label    string `json:"label"`
		Selector string `json:"selector"`
	}{Ref: "e1", Tag: "input", Type: "text", Label: "Name", Selector: "#n"})
	out := formatRead(r)
	for _, want := range []string{"T — http://x", "body text", `e1 <input:text> "Name"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

const testPage = `<!doctype html><html><head><title>Factor Test Page</title></head>
<body>
  <h1>Welcome</h1>
  <p>Some page content here.</p>
  <input id="q" type="text" placeholder="Search box">
  <button id="go" onclick="document.title='Clicked!'">Go</button>
  <a href="/other">Other page</a>
</body></html>`

func TestBrowserEndToEnd(t *testing.T) {
	if _, err := FindBrowserBinary(""); err != nil {
		t.Skipf("no chromium available: %v", err)
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			fmt.Fprint(w, `<html><head><title>Other</title></head><body>second page</body></html>`)
			return
		}
		fmt.Fprint(w, testPage)
	}))
	defer srv.Close()

	ws := t.TempDir()
	suite, closeFn := NewTools(config.BrowserConfig{
		Enabled:  true,
		Headless: true, // tests never pop a window
		// CI runners and containers restrict unprivileged user namespaces,
		// which stops Chrome from starting at all. The sandbox is not what
		// this test is about.
		NoSandbox:   true,
		UserDataDir: filepath.Join(ws, "profile"),
	}, ws)
	defer closeFn()
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	ctx := context.Background()

	res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL})
	if res.IsError {
		t.Fatalf("navigate: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Factor Test Page") || !strings.Contains(res.ForLLM, "Search box") {
		t.Fatalf("read output:\n%s", res.ForLLM)
	}

	// find the button's ref from the read output and click it
	var buttonRef string
	for _, line := range strings.Split(res.ForLLM, "\n") {
		if strings.Contains(line, `"Go"`) {
			buttonRef = strings.Fields(strings.TrimSpace(line))[0]
		}
	}
	if buttonRef == "" {
		t.Fatalf("no ref for Go button in:\n%s", res.ForLLM)
	}
	res = byName["browser_click"].Execute(ctx, map[string]any{"target": buttonRef})
	if res.IsError || !strings.Contains(res.ForLLM, "Clicked!") {
		t.Fatalf("click: %s", res.ForLLM)
	}

	res = byName["browser_fill"].Execute(ctx, map[string]any{"target": "#q", "text": "hello world"})
	if res.IsError {
		t.Fatalf("fill: %s", res.ForLLM)
	}
	res = byName["browser_eval"].Execute(ctx, map[string]any{"expression": "document.getElementById('q').value"})
	if res.IsError || !strings.Contains(res.ForLLM, "hello world") {
		t.Fatalf("eval after fill: %s", res.ForLLM)
	}

	res = byName["browser_screenshot"].Execute(ctx, map[string]any{})
	if res.IsError {
		t.Fatalf("screenshot: %s", res.ForLLM)
	}
	shots, _ := os.ReadDir(filepath.Join(ws, "screenshots"))
	if len(shots) != 1 {
		t.Errorf("screenshots on disk = %d", len(shots))
	}
}
