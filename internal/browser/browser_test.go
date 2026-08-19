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
	s := NewSession(config.BrowserConfig{}, t.TempDir(), nil)
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
	cfg := liveConfig()
	cfg.UserDataDir = filepath.Join(liveProfileDir(t), "profile")
	suite, closeFn := NewTools(cfg, ws, nil)
	defer closeFn()
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	ctx := context.Background()

	res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL})
	if res.IsError {
		// A machine that cannot launch Chrome at all (locked-down CI images,
		// containers without the right libraries) cannot exercise these
		// tools — the same situation as the missing-binary skip above.
		// Everything after a successful start is still a hard failure.
		if strings.Contains(res.ForLLM, "browser start failed") || strings.Contains(res.ForLLM, "browser start timed out") {
			t.Skipf("chrome cannot start in this environment: %s", res.ForLLM)
		}
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

func TestElementHeaderSaysWhatItLeftOut(t *testing.T) {
	shown := func(n int) []struct {
		Ref      string `json:"ref"`
		Tag      string `json:"tag"`
		Type     string `json:"type"`
		Label    string `json:"label"`
		Selector string `json:"selector"`
	} {
		return make([]struct {
			Ref      string `json:"ref"`
			Tag      string `json:"tag"`
			Type     string `json:"type"`
			Label    string `json:"label"`
			Selector string `json:"selector"`
		}, n)
	}
	for _, tc := range []struct {
		name       string
		r          pageRead
		want, deny string
	}{
		{"the whole page fits", pageRead{Elements: shown(5), MatchTotal: 5, ElementTotal: 5}, "5 shown;", "on the page"},
		{"more than fit", pageRead{Elements: shown(100), MatchTotal: 422, ElementTotal: 422},
			"100 shown of 422 on the page — narrow with filter, or raise limit", ""},
		{"a filter that still overflows", pageRead{Elements: shown(3), MatchTotal: 12, ElementTotal: 400},
			"3 shown of 12 matching, 400 on the page", ""},
		{"a filter that fits", pageRead{Elements: shown(12), MatchTotal: 12, ElementTotal: 400},
			"12 shown of 400 on the page, filtered", ""},
	} {
		got := elementHeader(&tc.r)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: header = %q, want it to contain %q", tc.name, got, tc.want)
		}
		if tc.deny != "" && strings.Contains(got, tc.deny) {
			t.Errorf("%s: header = %q, should not mention %q", tc.name, got, tc.deny)
		}
	}
}

func TestFormatReadReportsATruncatedPage(t *testing.T) {
	full := formatRead(&pageRead{Title: "T", URL: "u", Text: "abc", TextTotal: 3})
	if strings.Contains(full, "characters") || strings.Contains(full, "main content region") {
		t.Errorf("a complete read claimed to be partial:\n%s", full)
	}
	cut := formatRead(&pageRead{Title: "T", URL: "u", Text: "abc", TextTotal: 9000, FromMain: true})
	if !strings.Contains(cut, "showing 3 of 9000 characters") {
		t.Errorf("a truncated read did not say so:\n%s", cut)
	}
	if !strings.Contains(cut, "main content region") {
		t.Errorf("a read from the main region did not say where it came from:\n%s", cut)
	}
}

func TestElementLimitStaysInsideWhatOneReadCanCarry(t *testing.T) {
	for in, want := range map[int]int{
		0: defaultElementLimit, -5: defaultElementLimit,
		25: 25, maxElementLimit: maxElementLimit, 10000: maxElementLimit,
	} {
		if got := elementLimit(in); got != want {
			t.Errorf("elementLimit(%d) = %d, want %d", in, got, want)
		}
	}
}
