//go:build !nobrowser

package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// The suite's tests run wherever CI puts them, which is usually a box with
// no display — exactly where "auto" chooses Camofox. None of them may
// download Node or an npm package to find that out, so the provisioner is
// stubbed for the whole package: "auto" then falls back to the Chromium
// path the older tests drive, and the Camofox tests name their engine and
// point it at a fake server.
func init() {
	provisionCamofox = func(context.Context, string, Progress) (string, bool, error) {
		return "", false, errors.New("not in tests")
	}
	// Nor may a test find the developer's own install and start it.
	home, err := os.MkdirTemp("", "factor-browser-test-home")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("FACTOR_HOME", home)
}

// fakeCamofox is the server's REST surface, enough of it to prove every tool
// sends what the server reads and renders what it answers.
type fakeCamofox struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string // "METHOD path body"
	tabs     map[string]string
	nextTab  int
	snapshot string
	title    string
	failWith map[string]string // path suffix -> JSON error body
}

func newFakeCamofox(t *testing.T) (*fakeCamofox, *httptest.Server) {
	f := &fakeCamofox{t: t, tabs: map[string]string{}, snapshot: "- main:\n  - heading \"Hello\" [level=1]\n  - link \"Next\" [e1]\n  - textbox \"Search\" [e2]\n", title: "Test Page", failWith: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCamofox) record(r *http.Request) map[string]any {
	body := map[string]any{}
	raw, _ := io.ReadAll(r.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" "+string(raw))
	f.mu.Unlock()
	return body
}

func (f *fakeCamofox) serve(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	for suffix, errBody := range f.failWith {
		if strings.HasSuffix(r.URL.Path, suffix) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(errBody))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/health":
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.URL.Path == "/tabs" && r.Method == http.MethodPost:
		if body["userId"] != camofoxUser || body["sessionKey"] != camofoxUser {
			http.Error(w, `{"error":"userId and sessionKey required"}`, 400)
			return
		}
		f.mu.Lock()
		f.nextTab++
		id := fmt.Sprintf("tab-%d", f.nextTab)
		f.tabs[id] = body["url"].(string)
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"tabId":%q,"url":%q}`, id, body["url"])
	case r.URL.Path == "/tabs" && r.Method == http.MethodGet:
		f.mu.Lock()
		ids := make([]string, 0, len(f.tabs))
		for id := range f.tabs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var items []string
		for _, id := range ids {
			items = append(items, fmt.Sprintf(`{"tabId":%q,"url":%q,"title":%q}`, id, f.tabs[id], f.title))
		}
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"running":true,"tabs":[%s]}`, strings.Join(items, ","))
	case strings.HasPrefix(r.URL.Path, "/tabs/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/tabs/"), "/")
		id := parts[0]
		f.mu.Lock()
		url, ok := f.tabs[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Tab not found","code":"tab_not_found"}`))
			return
		}
		if len(parts) == 1 && r.Method == http.MethodDelete {
			f.mu.Lock()
			delete(f.tabs, id)
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		switch parts[1] {
		case "navigate":
			f.mu.Lock()
			f.tabs[id] = body["url"].(string)
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"ok":true,"url":%q}`, body["url"])
		case "snapshot":
			if r.URL.Query().Get("userId") != camofoxUser {
				http.Error(w, `{"error":"userId required"}`, 400)
				return
			}
			snap, _ := json.Marshal(f.snapshot)
			_, _ = fmt.Fprintf(w, `{"url":%q,"snapshot":%s,"refsCount":2,"truncated":false,"totalChars":%d}`, url, snap, len(f.snapshot))
		case "evaluate":
			if body["expression"] == "document.title" {
				_, _ = fmt.Fprintf(w, `{"ok":true,"result":%q}`, f.title)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"answer":42}}`))
		case "screenshot":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG fake"))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCamofox) saw(fragment string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r, fragment) {
			return true
		}
	}
	return false
}

// camofoxSuite builds the tool suite against a fake server, forced to the
// headless engine.
func camofoxSuite(t *testing.T, srv *httptest.Server) (map[string]tools.Tool, *fakeCamofox) {
	t.Helper()
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	cfg := config.BrowserConfig{Engine: "camofox", Camofox: config.CamofoxConfig{Port: port}}
	suite, closeFn := NewTools(cfg, t.TempDir(), nil)
	t.Cleanup(closeFn)
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	return byName, nil
}

func run(t *testing.T, tool tools.Tool, args map[string]any) *tools.Result {
	t.Helper()
	return tool.Execute(context.Background(), args)
}

func TestCamofoxNavigateOpensATabAndReadsTheSnapshot(t *testing.T) {
	f, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)

	res := run(t, suite["browser_navigate"], map[string]any{"url": "example.com"})
	if res.IsError {
		t.Fatalf("navigate: %s", res.ForLLM)
	}
	for _, want := range []string{"Test Page — https://example.com", `link "Next" [e1]`, `textbox "Search" [e2]`} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("read lacks %q:\n%s", want, res.ForLLM)
		}
	}
	if !f.saw(`POST /tabs {"sessionKey":"factor","url":"https://example.com","userId":"factor"}`) {
		t.Errorf("the tab was not created as the server wants it: %v", f.requests)
	}

	// The second navigation reuses the tab rather than opening another.
	run(t, suite["browser_navigate"], map[string]any{"url": "https://example.com/two"})
	if !f.saw("POST /tabs/tab-1/navigate") || len(f.tabs) != 1 {
		t.Errorf("second navigation opened a tab: %v", f.requests)
	}
}

func TestCamofoxRefusesLocalFiles(t *testing.T) {
	_, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	res := run(t, suite["browser_navigate"], map[string]any{"url": "/etc/hosts"})
	if !res.IsError || !strings.Contains(res.ForLLM, "read_file") {
		t.Errorf("a local file was not refused with the alternative named: %+v", res)
	}
}

func TestCamofoxToolsNeedAPageFirst(t *testing.T) {
	_, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	for _, name := range []string{"browser_read", "browser_click", "browser_fill", "browser_keys", "browser_scroll", "browser_back", "browser_eval", "browser_screenshot"} {
		res := run(t, suite[name], map[string]any{"target": "e1", "text": "x", "keys": "Enter", "expression": "1"})
		if !res.IsError || !strings.Contains(res.ForLLM, "browser_navigate first") {
			t.Errorf("%s without a page = %+v", name, res)
		}
	}
}

func TestCamofoxActionsSendRefsSelectorsAndKeys(t *testing.T) {
	f, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	run(t, suite["browser_navigate"], map[string]any{"url": "https://example.com"})

	run(t, suite["browser_click"], map[string]any{"target": "e1"})
	run(t, suite["browser_click"], map[string]any{"target": "a.next"})
	run(t, suite["browser_fill"], map[string]any{"target": "e2", "text": "hello", "submit": true})
	run(t, suite["browser_keys"], map[string]any{"keys": "Control+Enter"})
	run(t, suite["browser_back"], map[string]any{})
	for _, want := range []string{
		`POST /tabs/tab-1/click {"ref":"e1","userId":"factor"}`,
		`POST /tabs/tab-1/click {"selector":"a.next","userId":"factor"}`,
		`POST /tabs/tab-1/type {"ref":"e2","submit":true,"text":"hello","userId":"factor"}`,
		`POST /tabs/tab-1/press {"key":"Control+Enter","userId":"factor"}`,
		`POST /tabs/tab-1/back {"userId":"factor"}`,
	} {
		if !f.saw(want) {
			t.Errorf("never sent %s\nsaw: %s", want, strings.Join(f.requests, "\n"))
		}
	}
}

func TestCamofoxScrollReachesTheEndsThroughThePage(t *testing.T) {
	f, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	run(t, suite["browser_navigate"], map[string]any{"url": "https://example.com"})
	run(t, suite["browser_scroll"], map[string]any{})
	run(t, suite["browser_scroll"], map[string]any{"to": "up"})
	run(t, suite["browser_scroll"], map[string]any{"to": "bottom"})
	for _, want := range []string{
		`POST /tabs/tab-1/scroll {"amount":800,"direction":"down","userId":"factor"}`,
		`POST /tabs/tab-1/scroll {"amount":800,"direction":"up","userId":"factor"}`,
		`window.scrollTo(0, document.documentElement.scrollHeight)`,
	} {
		if !f.saw(want) {
			t.Errorf("never sent %s\nsaw: %s", want, strings.Join(f.requests, "\n"))
		}
	}
}

func TestCamofoxEvalReturnsJSON(t *testing.T) {
	_, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	run(t, suite["browser_navigate"], map[string]any{"url": "https://example.com"})
	res := run(t, suite["browser_eval"], map[string]any{"expression": "({answer: 42})"})
	if res.IsError || res.ForLLM != `{"answer":42}` {
		t.Errorf("eval = %+v", res)
	}
}

func TestCamofoxScreenshotSavesAPNG(t *testing.T) {
	_, srv := newFakeCamofox(t)
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	ws := t.TempDir()
	suite, closeFn := NewTools(config.BrowserConfig{Engine: "camofox", Camofox: config.CamofoxConfig{Port: port}}, ws, nil)
	defer closeFn()
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	run(t, byName["browser_navigate"], map[string]any{"url": "https://example.com"})
	res := run(t, byName["browser_screenshot"], map[string]any{})
	if res.IsError || !strings.HasSuffix(res.ForUser, ".png") || !strings.HasPrefix(res.ForUser, filepath.Join(ws, "screenshots")) {
		t.Fatalf("screenshot = %+v", res)
	}
	data, err := os.ReadFile(res.ForUser)
	if err != nil || !bytes.HasPrefix(data, []byte("\x89PNG")) {
		t.Errorf("saved file = %q, %v", data, err)
	}
}

func TestCamofoxServerErrorsReachTheModel(t *testing.T) {
	f, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	run(t, suite["browser_navigate"], map[string]any{"url": "https://example.com"})
	f.failWith["/click"] = `{"error":"Unknown ref: e9 (valid refs: e1-e2). Refs reset after navigation - call snapshot first.","code":"stale_refs"}`
	res := run(t, suite["browser_click"], map[string]any{"target": "e9"})
	if !res.IsError || !strings.Contains(res.ForLLM, "Unknown ref: e9") {
		t.Errorf("click = %+v", res)
	}
}

func TestCamofoxLostTabIsReportedAndReopened(t *testing.T) {
	f, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	run(t, suite["browser_navigate"], map[string]any{"url": "https://example.com"})
	f.mu.Lock()
	delete(f.tabs, "tab-1") // Firefox was shut down idle
	f.mu.Unlock()
	res := run(t, suite["browser_read"], map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "browser_navigate again") {
		t.Errorf("read of a lost tab = %+v", res)
	}
	res = run(t, suite["browser_navigate"], map[string]any{"url": "https://example.com"})
	if res.IsError || !f.saw("POST /tabs {") {
		t.Errorf("a fresh tab was not opened: %+v", res)
	}
}

func TestCamofoxTabsListSwitchOpenClose(t *testing.T) {
	f, srv := newFakeCamofox(t)
	suite, _ := camofoxSuite(t, srv)
	run(t, suite["browser_navigate"], map[string]any{"url": "https://one.example"})
	res := run(t, suite["browser_tabs"], map[string]any{"action": "open", "url": "https://two.example"})
	if res.IsError || len(f.tabs) != 2 {
		t.Fatalf("open = %+v, tabs %v", res, f.tabs)
	}
	res = run(t, suite["browser_tabs"], map[string]any{"action": "list"})
	if !strings.Contains(res.ForLLM, "2 open tabs") || !strings.Contains(res.ForLLM, "* 2") {
		t.Errorf("list = %s", res.ForLLM)
	}
	res = run(t, suite["browser_tabs"], map[string]any{"action": "switch", "target": "one.example"})
	if res.IsError || !strings.Contains(res.ForLLM, "https://one.example") {
		t.Errorf("switch = %+v", res)
	}
	res = run(t, suite["browser_tabs"], map[string]any{"action": "close", "target": "2"})
	if res.IsError || len(f.tabs) != 1 {
		t.Errorf("close = %+v, tabs %v", res, f.tabs)
	}
	res = run(t, suite["browser_tabs"], map[string]any{"action": "switch", "target": "nowhere"})
	if !res.IsError {
		t.Errorf("switching to nothing succeeded: %+v", res)
	}
	res = run(t, suite["browser_tabs"], map[string]any{"action": "dance"})
	if !res.IsError {
		t.Errorf("an unknown action succeeded: %+v", res)
	}
}

func TestCamofoxUploadHandsOverAWorkspaceRelativePath(t *testing.T) {
	f, srv := newFakeCamofox(t)
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	ws := t.TempDir()
	guard := tools.NewPathGuard(ws, true, false, nil)
	suite, closeFn := NewTools(config.BrowserConfig{Engine: "camofox", Camofox: config.CamofoxConfig{Port: port}}, ws, guard)
	defer closeFn()
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	if err := os.WriteFile(filepath.Join(ws, "cv.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, byName["browser_navigate"], map[string]any{"url": "https://example.com"})
	res := run(t, byName["browser_upload"], map[string]any{"path": "cv.pdf", "target": "e2"})
	if res.IsError {
		t.Fatalf("upload = %s", res.ForLLM)
	}
	if !f.saw(`POST /tabs/tab-1/upload {"path":"cv.pdf","ref":"e2","userId":"factor"}`) {
		t.Errorf("upload sent %v", f.requests)
	}
	res = run(t, byName["browser_upload"], map[string]any{"path": "/etc/hosts"})
	if !res.IsError {
		t.Errorf("a file outside the workspace was accepted: %+v", res)
	}
}

func TestFormatSnapshotFiltersLimitsAndSaysWhatItCut(t *testing.T) {
	snap := camofoxSnapshot{URL: "https://x", Snapshot: "- link \"A\" [e1]\n- link \"B\" [e2]\n- text: plain\n- link \"C\" [e3]\n", Truncated: true, TotalChars: 5000}
	out := formatSnapshot("T", snap, "", 2, 0)
	if !strings.Contains(out, "[e1]") || !strings.Contains(out, "[e2]") || strings.Contains(out, "[e3]") {
		t.Errorf("limit not applied:\n%s", out)
	}
	if !strings.Contains(out, "1 more elements not listed") || !strings.Contains(out, "offset=") {
		t.Errorf("cuts not stated:\n%s", out)
	}
	out = formatSnapshot("", snap, "plain", 0, 0)
	if !strings.Contains(out, "text: plain") || strings.Contains(out, "[e1]") || !strings.HasPrefix(out, "https://x") {
		t.Errorf("filter not applied:\n%s", out)
	}
	out = formatSnapshot("T", camofoxSnapshot{URL: "https://x"}, "zzz", 0, 0)
	if !strings.Contains(out, `nothing on the page mentions "zzz"`) {
		t.Errorf("empty filter result not explained:\n%s", out)
	}
}

// "auto" picks the headless engine only where a window cannot open, and
// never over a browser the user already has attached.
func TestEnginePolicy(t *testing.T) {
	_, srv := newFakeCamofox(t)
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	dead := devtoolsProbe
	devtoolsProbe = "http://127.0.0.1:1"
	t.Cleanup(func() { devtoolsProbe = dead })

	pick := func(cfg config.BrowserConfig) bool {
		cfg.Camofox.Port = port
		s := NewSession(cfg, t.TempDir(), nil)
		defer s.Close()
		return s.engine(context.Background()) != nil
	}
	if !pick(config.BrowserConfig{Engine: "camofox"}) {
		t.Error("forced camofox was not chosen")
	}
	if pick(config.BrowserConfig{Engine: "chromium", Headless: true}) {
		t.Error("forced chromium chose camofox")
	}
	if !pick(config.BrowserConfig{Engine: "auto", Headless: true}) {
		t.Error("auto with headless did not choose camofox")
	}
	if pick(config.BrowserConfig{Engine: "auto", Headless: true, AttachURL: "http://127.0.0.1:1"}) {
		t.Error("auto chose camofox over an attached browser")
	}
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	if runtime.GOOS == "linux" && pick(config.BrowserConfig{Engine: "auto"}) {
		t.Error("auto chose camofox with a display available")
	}
	t.Setenv("DISPLAY", "")
	if runtime.GOOS == "linux" && !pick(config.BrowserConfig{Engine: "auto"}) {
		t.Error("auto with no display did not choose camofox")
	}
}

// With nothing answering and nothing installable, "auto" hands the session
// to the Chromium engine; forced by name the failure stands.
func TestAutoFallsBackWhenCamofoxCannotStart(t *testing.T) {
	dead := devtoolsProbe
	devtoolsProbe = "http://127.0.0.1:1"
	t.Cleanup(func() { devtoolsProbe = dead })
	s := NewSession(config.BrowserConfig{Engine: "auto", Headless: true, Camofox: config.CamofoxConfig{Port: 1}}, t.TempDir(), nil)
	defer s.Close()
	if s.engine(context.Background()) != nil {
		t.Error("auto kept an engine that cannot start")
	}
	forced := NewSession(config.BrowserConfig{Engine: "camofox", Camofox: config.CamofoxConfig{Port: 1}}, t.TempDir(), nil)
	defer forced.Close()
	if forced.engine(context.Background()) == nil {
		t.Error("forced camofox fell back")
	}
	res := forced.cam.navigate(context.Background(), "https://example.com")
	if !res.IsError || !strings.Contains(res.ForLLM, "not installed") {
		t.Errorf("forced failure = %+v", res)
	}
}

func TestCamofoxEnvKeepsEverythingUnderFactor(t *testing.T) {
	cfg := config.BrowserConfig{UserDataDir: "/data/browser", Camofox: config.CamofoxConfig{Port: 9377}}
	c := newCamofox(cfg, "/home/x/.factor", "/ws", nil)
	env := strings.Join(c.env(), "\n")
	for _, want := range []string{
		"CAMOFOX_PORT=9377", "CAMOFOX_BIND_HOST=127.0.0.1", "CAMOFOX_CRASH_REPORT_ENABLED=false",
		"CAMOUFOX_INSTALL_DIR=" + filepath.Join("/home/x/.factor", "engine", "camofox", "camoufox"),
		"CAMOFOX_PROFILE_DIR=" + filepath.Join("/data/browser", "camofox", "profiles"),
		"CAMOFOX_UPLOADS_DIR=/ws",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}

// The whole path for real: Factor's own installer puts the engine down, the
// session starts it, and a page is read and clicked through it. It costs a
// 600 MB download the first time, so it only runs when asked for by name,
// into the home it is pointed at (the developer's own by default, so the
// download happens once).
func TestLiveCamofoxReadsARealPage(t *testing.T) {
	if os.Getenv("FACTOR_TEST_CAMOFOX") == "" {
		t.Skip("set FACTOR_TEST_CAMOFOX=1 to install and drive the real engine")
	}
	home := os.Getenv("FACTOR_TEST_CAMOFOX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		home = filepath.Join(userHome, ".factor")
	}
	ctx, cancel := context.WithTimeout(context.Background(), InstallTimeout)
	defer cancel()
	server, installed, err := EnsureCamofox(ctx, home, func(f string, a ...any) { t.Logf(f, a...) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("server %s (installed now: %v)", server, installed)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			_, _ = w.Write([]byte(`<html><head><title>Second</title></head><body><main><h1>You clicked</h1></main></body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>Live Test</title></head><body><main><h1>Hello Camofox</h1><p>Body text.</p><a href="/next">Next page</a><input placeholder="Search"></main></body></html>`))
	}))
	defer page.Close()

	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACTOR_HOME", home)
	cfg := config.BrowserConfig{Engine: "camofox", UserDataDir: t.TempDir(), Camofox: config.CamofoxConfig{Port: port}}
	suite, closeFn := NewTools(cfg, t.TempDir(), nil)
	defer closeFn()
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}
	res := run(t, byName["browser_navigate"], map[string]any{"url": page.URL})
	if res.IsError {
		t.Fatalf("navigate: %s", res.ForLLM)
	}
	t.Logf("read:\n%s", res.ForLLM)
	for _, want := range []string{"Live Test", "Hello Camofox", `"Next page"`, `"Search"`} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("read lacks %q", want)
		}
	}
	res = run(t, byName["browser_click"], map[string]any{"target": "e1"})
	if res.IsError || !strings.Contains(res.ForLLM, "You clicked") {
		t.Errorf("click = %+v", res)
	}
	res = run(t, byName["browser_eval"], map[string]any{"expression": "navigator.userAgent"})
	if res.IsError || !strings.Contains(res.ForLLM, "Firefox") {
		t.Errorf("eval = %+v", res)
	}
	res = run(t, byName["browser_screenshot"], map[string]any{})
	if res.IsError {
		t.Errorf("screenshot = %+v", res)
	}
}

// freeLoopbackPort asks the kernel for a port nothing holds.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
