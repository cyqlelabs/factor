//go:build !nobrowser

package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// requireBrowser skips when the machine has no browser for the suite to drive,
// so a CI box without Chrome still runs everything else in this package.
func requireBrowser(t *testing.T) {
	t.Helper()
	if _, err := FindBrowserBinary(""); err != nil {
		t.Skipf("no chromium available: %v", err)
	}
	if testing.Short() {
		t.Skip("short mode")
	}
}

// servePage starts a local site with the shared test page at / and a second
// document at /other. no-store keeps the pages out of the back/forward cache,
// so going back is a real navigation the browser reports finishing.
func servePage(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/other" {
			fmt.Fprint(w, `<html><head><title>Other</title></head><body>second page</body></html>`)
			return
		}
		fmt.Fprint(w, testPage)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMain points the auto-attach probe at a dead port for the whole package.
// A developer running these tests usually has their own browser open with a
// DevTools port, and without this the live tests would attach to it and drive
// their real tabs instead of a fixture.
func TestMain(m *testing.M) {
	devtoolsProbe = "http://127.0.0.1:1"
	os.Exit(m.Run())
}

// liveConfig is the browser setup every live test starts from. CI runners and
// containers restrict unprivileged user namespaces, which stops Chrome from
// starting at all; the sandbox is not what these tests are about.
func liveConfig() config.BrowserConfig {
	return config.BrowserConfig{
		Enabled:   true,
		Headless:  true, // tests never pop a window
		NoSandbox: true,
	}
}

// liveProfileDir returns a directory for a browser profile, outside t.TempDir:
// a browser being torn down keeps writing to its profile for a moment, which
// would fail the strict cleanup t.TempDir does.
func liveProfileDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "factor-profile")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// liveSuite starts a browser-backed tool suite keyed by tool name. No user
// data dir is configured, so chromedp owns (and cleans up) the throwaway
// profile of any browser it launches.
func liveSuite(t *testing.T) map[string]tools.Tool {
	t.Helper()
	ws := t.TempDir()
	suite, closeFn := NewTools(liveConfig(), ws, nil)
	t.Cleanup(closeFn)
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	return byName
}

func TestBrowserReadWorksBeforeAnyNavigation(t *testing.T) {
	requireBrowser(t)
	res := liveSuite(t)["browser_read"].Execute(context.Background(), nil)
	if res.IsError {
		t.Fatalf("read: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "about:blank") {
		t.Errorf("fresh tab read does not report the blank page:\n%s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Interactive elements") {
		t.Errorf("read output is not formatted:\n%s", res.ForLLM)
	}
}

func TestBrowserBackReturnsToThePreviousPage(t *testing.T) {
	requireBrowser(t)
	srv := servePage(t)
	byName := liveSuite(t)
	ctx := context.Background()

	res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL})
	if res.IsError {
		t.Fatalf("first navigate: %s", res.ForLLM)
	}
	res = byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL + "/other"})
	if res.IsError || !strings.Contains(res.ForLLM, "second page") {
		t.Fatalf("second navigate: %s", res.ForLLM)
	}

	res = byName["browser_back"].Execute(ctx, nil)
	if res.IsError {
		t.Fatalf("back: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Factor Test Page") {
		t.Errorf("back did not land on the first page:\n%s", res.ForLLM)
	}
}

func TestBrowserClickAndFillRejectSelectorsThatMatchNothing(t *testing.T) {
	requireBrowser(t)
	srv := servePage(t)
	byName := liveSuite(t)
	ctx := context.Background()

	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL}); res.IsError {
		t.Fatalf("navigate: %s", res.ForLLM)
	}

	res := byName["browser_click"].Execute(ctx, map[string]any{"target": "#not-on-this-page"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no element matches") {
		t.Errorf("click result = %+v", res)
	}
	res = byName["browser_fill"].Execute(ctx, map[string]any{"target": "#not-on-this-page", "text": "hello"})
	if !res.IsError || !strings.Contains(res.ForLLM, "no element matches") {
		t.Errorf("fill result = %+v", res)
	}
	// A stale ref resolves to itself and fails the same way.
	res = byName["browser_click"].Execute(ctx, map[string]any{"target": "e99"})
	if !res.IsError || !strings.Contains(res.ForLLM, `no element matches "e99"`) {
		t.Errorf("stale ref result = %+v", res)
	}
}

func TestBrowserEvalReturnsValuesAndReportsExceptions(t *testing.T) {
	requireBrowser(t)
	byName := liveSuite(t)
	ctx := context.Background()

	res := byName["browser_eval"].Execute(ctx, map[string]any{"expression": "40 + 2"})
	if res.IsError || res.ForLLM != "42" {
		t.Errorf("eval result = %+v", res)
	}
	res = byName["browser_eval"].Execute(ctx, map[string]any{"expression": `"a".concat("b")`})
	if res.IsError || res.ForLLM != "ab" {
		t.Errorf("eval string result = %+v", res)
	}

	res = byName["browser_eval"].Execute(ctx, map[string]any{"expression": `throw new Error("boom")`})
	if !res.IsError {
		t.Fatalf("a throwing expression returned success: %q", res.ForLLM)
	}
	for _, want := range []string{"eval failed", "boom"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("eval error %q does not mention %q", res.ForLLM, want)
		}
	}
}

// A URL with no scheme is sent to https, so the plain-HTTP test server — which
// serves the very same host:port fine over http:// — cannot answer it.
func TestBrowserNavigateAssumesHttpsForSchemelessURLs(t *testing.T) {
	requireBrowser(t)
	srv := servePage(t)
	byName := liveSuite(t)
	ctx := context.Background()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": "http://" + hostPort}); res.IsError {
		t.Fatalf("the server does answer over http://: %s", res.ForLLM)
	}

	res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": hostPort})
	if !res.IsError {
		t.Fatalf("scheme-less URL was not sent over https: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "navigate failed") {
		t.Errorf("navigate error = %q", res.ForLLM)
	}
}

func TestBrowserBackFailsWithNoHistoryToGoBackTo(t *testing.T) {
	requireBrowser(t)
	res := liveSuite(t)["browser_back"].Execute(context.Background(), nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "back failed") {
		t.Errorf("back on a fresh tab = %+v", res)
	}
}

// An unparseable selector is a different failure from one that simply matches
// nothing, and the tools must say so rather than claim the element is missing.
func TestBrowserClickAndFillReportInvalidSelectors(t *testing.T) {
	requireBrowser(t)
	srv := servePage(t)
	byName := liveSuite(t)
	ctx := context.Background()

	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL}); res.IsError {
		t.Fatalf("navigate: %s", res.ForLLM)
	}

	res := byName["browser_click"].Execute(ctx, map[string]any{"target": ":::"})
	if !res.IsError || !strings.Contains(res.ForLLM, `click ":::" failed`) {
		t.Errorf("click result = %+v", res)
	}
	res = byName["browser_fill"].Execute(ctx, map[string]any{"target": ":::", "text": "hello"})
	if !res.IsError || !strings.Contains(res.ForLLM, `fill ":::" failed`) {
		t.Errorf("fill result = %+v", res)
	}
}

func TestBrowserScreenshotReportsWorkspaceFailures(t *testing.T) {
	requireBrowser(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	blocked := filepath.Join(root, "workspace-is-a-file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSession(liveConfig(), blocked, nil)
	defer s.Close()
	shot := &screenshotTool{s}
	ctx := context.Background()

	if res := shot.Execute(ctx, nil); !res.IsError {
		t.Errorf("screenshot into a file-shaped workspace succeeded: %q", res.ForLLM)
	}

	readOnly := filepath.Join(root, "read-only")
	shots := filepath.Join(readOnly, "screenshots")
	if err := os.MkdirAll(shots, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shots, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shots, 0o755) })
	s.workspace = readOnly
	if res := shot.Execute(ctx, nil); !res.IsError {
		t.Errorf("screenshot into a read-only directory succeeded: %q", res.ForLLM)
	}
}

func TestRunStopsWhenTheCallerCancels(t *testing.T) {
	requireBrowser(t)
	ws := t.TempDir()
	s := NewSession(liveConfig(), ws, nil)
	defer s.Close()
	if _, err := s.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.run(ctx, 20*time.Second, chromedp.Sleep(2*time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("run error = %v, want %v", err, context.Canceled)
	}
}

// The launch path is what a plain install uses; blocking the DevTools probe
// keeps this test off whatever browser the developer happens to be running.
func TestSessionLaunchesAManagedBrowserWhenNoDevToolsPortIsOpen(t *testing.T) {
	requireBrowser(t)
	blockDevToolsProbe(t)
	srv := servePage(t)
	profile := liveProfileDir(t)
	cfg := liveConfig()
	cfg.UserDataDir = filepath.Join(profile, "nested")
	s := NewSession(cfg, t.TempDir(), nil)
	defer s.Close()

	res := (&navigateTool{s}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if res.IsError {
		t.Fatalf("navigate in a launched browser: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Factor Test Page") {
		t.Errorf("read output:\n%s", res.ForLLM)
	}
	// The configured profile directory is created on demand and used.
	if entries, err := os.ReadDir(filepath.Join(profile, "nested")); err != nil || len(entries) == 0 {
		t.Errorf("configured user data dir was not used: %v (%d entries)", err, len(entries))
	}
}

func TestSessionStartsAFreshBrowserAfterClose(t *testing.T) {
	requireBrowser(t)
	srv := servePage(t)
	ws := t.TempDir()
	s := NewSession(liveConfig(), ws, nil)
	defer s.Close()

	nav := &navigateTool{s}
	ctx := context.Background()
	if res := nav.Execute(ctx, map[string]any{"url": srv.URL}); res.IsError {
		t.Fatalf("first navigate: %s", res.ForLLM)
	}
	s.Close()
	if res := nav.Execute(ctx, map[string]any{"url": srv.URL}); res.IsError {
		t.Fatalf("navigate after close: %s", res.ForLLM)
	}
}
