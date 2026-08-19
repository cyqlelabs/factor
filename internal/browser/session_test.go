//go:build !nobrowser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// fakeBrowserOnPath drops executable stubs with the given names into a fresh
// directory and makes it the only PATH entry, so binary lookup is decided by
// the test rather than by whatever the host machine has installed.
func fakeBrowserOnPath(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func TestFindBrowserBinaryResolvesTheConfiguredCommand(t *testing.T) {
	dir := fakeBrowserOnPath(t, "my-browser")
	got, err := FindBrowserBinary("my-browser")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if want := filepath.Join(dir, "my-browser"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestFindBrowserBinaryFailsOnAMissingConfiguredCommand(t *testing.T) {
	fakeBrowserOnPath(t) // empty PATH directory
	if got, err := FindBrowserBinary("factor-nonexistent-browser"); err == nil {
		t.Fatalf("found %q, want an error", got)
	}
}

func TestFindBrowserBinaryAutoDetectsAChromiumFamilyBrowser(t *testing.T) {
	// google-chrome is also present, but chromium comes first in the candidate list.
	dir := fakeBrowserOnPath(t, "google-chrome", "chromium")
	got, err := FindBrowserBinary("")
	if err != nil {
		t.Fatalf("auto-detect: %v", err)
	}
	if want := filepath.Join(dir, "chromium"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestFindBrowserBinaryReportsTheCandidatesItLookedFor(t *testing.T) {
	noBrowserAnywhere(t)
	_, err := FindBrowserBinary("")
	if err == nil {
		t.Fatal("expected an error with no browser on PATH")
	}
	for _, want := range []string{"no Chromium-family browser found", "chromium", "brave-browser"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDevtoolsAlive(t *testing.T) {
	var mu sync.Mutex
	var probed []string
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probed = append(probed, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"Browser":"Chrome/127.0.0.0","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/x"}`)
	}))
	defer live.Close()

	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()

	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := unreachable.URL
	unreachable.Close()

	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"a DevTools endpoint answers", live.URL, true},
		{"a server without the endpoint", missing.URL, false},
		{"nothing is listening", unreachableURL, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := devtoolsAlive(tc.url); got != tc.want {
				t.Errorf("devtoolsAlive(%s) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(probed) != 1 || probed[0] != "/json/version" {
		t.Errorf("probed paths = %v, want [/json/version]", probed)
	}
}

// roundTripFunc turns a function into an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// blockDevToolsProbe makes ensure's probe of 127.0.0.1:9222 fail, so the test
// takes the launch path whether or not the machine running it happens to have
// a browser listening on the standard DevTools port.
func blockDevToolsProbe(t *testing.T) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("devtools probe blocked by test")
	})
	t.Cleanup(func() { http.DefaultTransport = original })
}

func TestSessionEnsureExplainsHowToRecoverWithoutABrowser(t *testing.T) {
	blockDevToolsProbe(t)
	s := NewSession(config.BrowserConfig{Command: "factor-nonexistent-browser"}, t.TempDir(), nil)
	defer s.Close()

	_, err := s.ensure()
	if err == nil {
		t.Fatal("expected ensure to fail with no browser to launch")
	}
	for _, want := range []string{"factor-nonexistent-browser", "attach_url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestSessionEnsureReportsAFailedAttach(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	// NOTE: this session is deliberately left unclosed and never re-ensured.
	// chromedp's cancel func may only be called once when allocation failed,
	// and ensure has already called it on this session's behalf; a second call
	// (from Close, or from the next tool call) blocks forever.
	s := NewSession(config.BrowserConfig{AttachURL: deadURL}, t.TempDir(), nil)

	_, err := s.ensure()
	if err == nil {
		t.Fatal("attaching to a dead endpoint succeeded")
	}
	if !strings.Contains(err.Error(), "browser start failed") {
		t.Errorf("attach error = %v", err)
	}
}

// Tools must not hang or panic when the browser cannot be started; they should
// hand the model the same recovery hint ensure produces.
func TestBrowserToolsExplainWhenNoBrowserCanStart(t *testing.T) {
	blockDevToolsProbe(t)
	s := NewSession(config.BrowserConfig{Command: "factor-nonexistent-browser"}, t.TempDir(), nil)
	defer s.Close() // safe here: nothing was allocated before the failure

	res := (&readTool{s}).Execute(context.Background(), nil)
	if !res.IsError {
		t.Fatalf("read succeeded without a browser: %q", res.ForLLM)
	}
	for _, want := range []string{"read failed", "attach_url"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("error %q does not mention %q", res.ForLLM, want)
		}
	}
}

func TestSessionCloseIsSafeBeforeUseAndWhenRepeated(t *testing.T) {
	s := NewSession(config.BrowserConfig{}, t.TempDir(), nil)
	s.Close()
	s.Close()

	_, closeFn := NewTools(config.BrowserConfig{}, t.TempDir(), nil)
	closeFn()
	closeFn()
}

func TestBrowserToolsDeclareUsableSchemas(t *testing.T) {
	suite, closeFn := NewTools(config.BrowserConfig{}, t.TempDir(), nil)
	defer closeFn()

	want := []string{
		"browser_navigate", "browser_read", "browser_scroll", "browser_click", "browser_fill",
		"browser_screenshot", "browser_eval", "browser_back",
		"browser_tabs", "browser_upload", "browser_keys",
	}
	if len(suite) != len(want) {
		t.Fatalf("suite size = %d, want %d", len(suite), len(want))
	}
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	for _, name := range want {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing tool %q", name)
		}
	}

	for _, tool := range suite {
		if strings.TrimSpace(tool.Description()) == "" {
			t.Errorf("%s has no description", tool.Name())
		}
		params := tool.Parameters()
		if params["type"] != "object" {
			t.Errorf("%s schema type = %v, want object", tool.Name(), params["type"])
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties object", tool.Name())
		}
		for name, prop := range props {
			p, ok := prop.(map[string]any)
			if !ok || p["type"] == nil || p["type"] == "" {
				t.Errorf("%s property %q has no type: %v", tool.Name(), name, prop)
			}
		}
		if req, present := params["required"]; present {
			list, ok := req.([]any)
			if !ok {
				t.Fatalf("%s required = %T, want []any", tool.Name(), req)
			}
			for _, entry := range list {
				name, ok := entry.(string)
				if !ok {
					t.Fatalf("%s required entry %v is not a string", tool.Name(), entry)
				}
				if _, declared := props[name]; !declared {
					t.Errorf("%s requires undeclared property %q", tool.Name(), name)
				}
			}
		}
		if _, err := json.Marshal(params); err != nil {
			t.Errorf("%s schema is not JSON-encodable: %v", tool.Name(), err)
		}
	}
}
