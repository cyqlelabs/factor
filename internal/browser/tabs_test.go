//go:build !nobrowser

package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/target"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

func TestResolveTabByIndexAndText(t *testing.T) {
	s := NewSession(config.BrowserConfig{}, t.TempDir(), nil)
	tabs := []tabInfo{
		{Index: 1, ID: "A", Title: "Inbox — cyqlelabs", URL: "https://mail.google.com/u/0"},
		{Index: 2, ID: "B", Title: "Inbox — nfiglesias", URL: "https://mail.google.com/u/1"},
		{Index: 3, ID: "C", Title: "News", URL: "https://infobae.com"},
	}

	got, err := s.resolveTab(tabs, "2")
	if err != nil || got.ID != "B" {
		t.Fatalf("by index = %v, %v", got.ID, err)
	}
	// Matching on text is the point: the number moves when the user opens a
	// tab mid-turn, the account name does not.
	got, err = s.resolveTab(tabs, "nfiglesias")
	if err != nil || got.ID != "B" {
		t.Fatalf("by title = %v, %v", got.ID, err)
	}
	got, err = s.resolveTab(tabs, "infobae.com")
	if err != nil || got.ID != "C" {
		t.Fatalf("by url = %v, %v", got.ID, err)
	}

	// An ambiguous match must name the candidates rather than guess: picking
	// the wrong Gmail account is exactly the failure this tool exists for.
	if _, err = s.resolveTab(tabs, "inbox"); err == nil || !strings.Contains(err.Error(), "matches 2 tabs") {
		t.Errorf("ambiguous match err = %v", err)
	}
	if _, err = s.resolveTab(tabs, "9"); err == nil {
		t.Error("out-of-range index accepted")
	}
	if _, err = s.resolveTab(tabs, ""); err == nil {
		t.Error("empty query accepted")
	}
}

func TestListTabsNumberingIsRecorded(t *testing.T) {
	s := NewSession(config.BrowserConfig{}, t.TempDir(), nil)
	s.tabRefs = map[int]target.ID{1: "stale"}
	if got := s.currentID(); got != "" {
		t.Errorf("currentID with no tab = %q", got)
	}
	s.cur = &tabHandle{id: "live"}
	if got := s.currentID(); got != "live" {
		t.Errorf("currentID = %q", got)
	}
}

func TestInterestingSkipsNonPages(t *testing.T) {
	cases := []struct {
		info *target.Info
		want bool
	}{
		{&target.Info{Type: "page", URL: "https://example.com"}, true},
		{&target.Info{Type: "page", URL: "about:blank"}, true},
		{&target.Info{Type: "service_worker", URL: "https://example.com/sw.js"}, false},
		{&target.Info{Type: "page", URL: "devtools://devtools/bundled/x.html"}, false},
		{&target.Info{Type: "page", URL: "chrome-extension://abc/popup.html"}, false},
		{&target.Info{Type: "page", URL: "chrome://settings"}, false},
	}
	for _, c := range cases {
		if got := interesting(c.info); got != c.want {
			t.Errorf("interesting(%s %s) = %v", c.info.Type, c.info.URL, got)
		}
	}
}

func TestFormatTabsMarksCurrent(t *testing.T) {
	out := formatTabs([]tabInfo{
		{Index: 1, Title: "One", URL: "https://one.example"},
		{Index: 2, Title: "Two", URL: "https://two.example", Current: true},
	})
	if !strings.Contains(out, `* 2 "Two"`) {
		t.Errorf("current tab not marked:\n%s", out)
	}
	if !strings.Contains(out, `  1 "One"`) {
		t.Errorf("other tab missing:\n%s", out)
	}
}

func TestFormatReadListsOtherTabs(t *testing.T) {
	r := &pageRead{Title: "T", URL: "http://x", OtherTabs: []string{`  2 "Inbox" https://mail.google.com`}}
	out := formatRead(r)
	if !strings.Contains(out, "Also open in this browser") || !strings.Contains(out, "mail.google.com") {
		t.Errorf("read does not surface the other tabs:\n%s", out)
	}
	// A browser with one tab must not grow a footer that says nothing.
	if strings.Contains(formatRead(&pageRead{Title: "T", URL: "http://x"}), "Also open") {
		t.Error("empty tab list still rendered a footer")
	}
}

func TestParseChord(t *testing.T) {
	mods, key, code, vk, err := parseChord("Control+Enter")
	if err != nil || mods != input.ModifierCtrl || key != "Enter" || code != "Enter" || vk != 13 {
		t.Fatalf("Control+Enter = %v %q %q %d %v", mods, key, code, vk, err)
	}
	if mods, key, _, _, err = parseChord("escape"); err != nil || mods != 0 || key != "Escape" {
		t.Errorf("escape = %v %q %v", mods, key, err)
	}
	if mods, _, _, _, err = parseChord("Shift+Tab"); err != nil || mods != input.ModifierShift {
		t.Errorf("Shift+Tab mods = %v %v", mods, err)
	}
	if _, key, code, _, err = parseChord("Ctrl+a"); err != nil || key != "a" || code != "KeyA" {
		t.Errorf("Ctrl+a = %q %q %v", key, code, err)
	}
	if _, _, code, _, err = parseChord("7"); err != nil || code != "Digit7" {
		t.Errorf("digit = %q %v", code, err)
	}
	if _, _, _, _, err = parseChord("Hyper+Enter"); err == nil {
		t.Error("unknown modifier accepted")
	}
	// Whole words belong in browser_fill; accepting them here would silently
	// send only the first letter.
	if _, _, _, _, err = parseChord("hello"); err == nil {
		t.Error("multi-character key accepted")
	}
	if _, _, _, _, err = parseChord("Control+"); err == nil {
		t.Error("missing key accepted")
	}
}

func TestUploadRefusesUnusableFiles(t *testing.T) {
	ws := t.TempDir()
	s := NewSession(config.BrowserConfig{}, ws, tools.NewPathGuard(ws, true, false, nil))
	up := &uploadTool{s}
	ctx := context.Background()

	if r := up.Execute(ctx, map[string]any{"path": filepath.Join(ws, "nope.html")}); !r.IsError {
		t.Error("missing file accepted")
	}
	empty := filepath.Join(ws, "empty.html")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// An empty attachment is the failure that looks like success: the mail
	// sends, and the file the user wanted is not in it.
	if r := up.Execute(ctx, map[string]any{"path": empty}); !r.IsError || !strings.Contains(r.ForLLM, "empty") {
		t.Errorf("empty file result = %+v", r)
	}
	if r := up.Execute(ctx, map[string]any{"path": ws}); !r.IsError || !strings.Contains(r.ForLLM, "directory") {
		t.Errorf("directory result = %+v", r)
	}
	// The guard is the same one the file tools use: a path it refuses to read
	// is a path that must not be uploaded either.
	if r := up.Execute(ctx, map[string]any{"path": "/etc/passwd"}); !r.IsError {
		t.Error("path outside the workspace accepted")
	}
}

func TestTabsToolRejectsUnknownAction(t *testing.T) {
	s := NewSession(config.BrowserConfig{}, t.TempDir(), nil)
	r := (&tabsTool{s}).Execute(context.Background(), map[string]any{"action": "teleport"})
	if !r.IsError || !strings.Contains(r.ForLLM, "unknown action") {
		t.Errorf("result = %+v", r)
	}
}
