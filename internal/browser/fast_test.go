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
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

func TestFastPathToolIsRegisteredOnlyWhenAskedFor(t *testing.T) {
	cfg := config.BrowserConfig{Enabled: true}
	suite, closeFn := NewTools(cfg, t.TempDir(), nil)
	defer closeFn()
	for _, tool := range suite {
		if tool.Name() == "browser_fetch" {
			t.Fatal("the lightweight engine's tool was registered without fast_path")
		}
	}

	cfg.FastPath = true
	suite, closeFn = NewTools(cfg, t.TempDir(), nil)
	defer closeFn()
	var found bool
	for _, tool := range suite {
		if tool.Name() == "browser_fetch" {
			found = true
		}
	}
	if !found {
		t.Error("fast_path is on but browser_fetch is missing")
	}
}

func TestFastFetchExplainsAMissingEngine(t *testing.T) {
	f := newFastSession(config.BrowserConfig{
		FastPath:    true,
		FastCommand: filepath.Join(t.TempDir(), "lightpanda"),
	})
	defer f.Close()
	res := (&fetchTool{f}).Execute(context.Background(), map[string]any{"url": "https://example.com"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not installed") {
		t.Errorf("res = %+v, want a clear report that the engine is missing", res)
	}
}

func TestFastFetchRequiresAURL(t *testing.T) {
	f := newFastSession(config.BrowserConfig{FastPath: true})
	defer f.Close()
	res := (&fetchTool{f}).Execute(context.Background(), map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "url is required") {
		t.Errorf("res = %+v", res)
	}
}

func TestFastFetchDeclaresAUsableSchema(t *testing.T) {
	tool := &fetchTool{newFastSession(config.BrowserConfig{})}
	if tool.Name() != "browser_fetch" || tool.Description() == "" {
		t.Errorf("name = %q", tool.Name())
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok || props["url"] == nil {
		t.Errorf("parameters = %+v, want a url property", params)
	}
	if _, ok := params["required"].([]any); !ok {
		t.Errorf("parameters = %+v, want url required", params)
	}
}

func TestFreePortReturnsSomethingBindable(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("port = %d", port)
	}
	// 9222 is the port the full browser attaches to; handing it out here
	// would make the two engines fight over one connection.
	if port == 9222 {
		t.Error("freePort handed out the DevTools default")
	}
}

func TestWaitForDevtoolsGivesUp(t *testing.T) {
	start := time.Now()
	err := waitForDevtools("http://127.0.0.1:1", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not open its CDP port") {
		t.Errorf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s past its own limit", elapsed)
	}
}

func TestWaitForDevtoolsAcceptsALiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:0/devtools/browser/x"}`)
	}))
	defer srv.Close()
	if err := waitForDevtools(srv.URL, 2*time.Second); err != nil {
		t.Errorf("waitForDevtools: %v", err)
	}
}

func TestFormatFastReadListsTextAndLinks(t *testing.T) {
	read := &fastPageRead{Title: "Cyqle", URL: "https://cyqle.dev", Text: "P2P Cloud Desktop"}
	read.Links = append(read.Links, struct {
		Text string `json:"text"`
		Href string `json:"href"`
	}{Text: "Pricing", Href: "https://cyqle.dev/pricing"})

	out := formatFastRead(read)
	for _, want := range []string{"Cyqle — https://cyqle.dev", "P2P Cloud Desktop", "Pricing — https://cyqle.dev/pricing"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestFastSessionCloseIsSafeBeforeUseAndWhenRepeated(t *testing.T) {
	f := newFastSession(config.BrowserConfig{})
	f.Close()
	f.Close()
}

// requireFastEngine skips unless a real Lightpanda is installed, the same way
// the Chromium live tests skip without a browser.
func requireFastEngine(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	path := os.Getenv("FACTOR_TEST_LIGHTPANDA")
	if path == "" {
		path = FastEngineBinary(config.Home())
	}
	if !executable(path) {
		t.Skipf("no lightweight engine at %s", path)
	}
	return path
}

func TestFastFetchReadsARealPage(t *testing.T) {
	binary := requireFastEngine(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><head><title>Factor Test Page</title></head><body>
			<p>hello from the fast path</p><a href="/next">Next page</a></body></html>`)
	}))
	defer srv.Close()

	f := newFastSession(config.BrowserConfig{FastPath: true, FastCommand: binary})
	defer f.Close()
	res := (&fetchTool{f}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if res.IsError {
		t.Fatalf("fetch: %s", res.ForLLM)
	}
	for _, want := range []string{"Factor Test Page", "hello from the fast path", "Next page"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("read %q missing %q", res.ForLLM, want)
		}
	}
	var _ tools.Tool = &fetchTool{f}
}
