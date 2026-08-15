//go:build !nobrowser

// Package browser gives the agent a real browser via the Chrome DevTools
// Protocol (chromedp): it attaches to the user's running Chrome/Chromium/
// Brave when a DevTools port is open, otherwise launches a managed instance
// — visible by default, so the user can watch the agent work. When the
// machine has no Chromium-family browser at all, `factor init` provisions one
// (see install.go). Build with -tags nobrowser to strip the whole suite from
// the binary.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Session lazily owns one browser connection shared by all browser tools.
type Session struct {
	cfg       config.BrowserConfig
	workspace string

	mu          sync.Mutex
	allocCancel context.CancelFunc
	tabCtx      context.Context
	tabCancel   context.CancelFunc
	refs        map[string]string // eN -> CSS selector from the last read
}

func NewSession(cfg config.BrowserConfig, workspace string) *Session {
	return &Session{cfg: cfg, workspace: workspace, refs: map[string]string{}}
}

var chromeCandidates = []string{"helium", "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave-browser", "microsoft-edge", "chrome"}

// FindBrowserBinary locates a Chromium-family binary: first on PATH, then in
// the fixed locations PATH never covers — where macOS and Windows keep their
// browsers, and where Factor installs the one it provisions itself.
func FindBrowserBinary(configured string) (string, error) {
	if configured != "" {
		return exec.LookPath(configured)
	}
	for _, c := range chromeCandidates {
		if path, err := exec.LookPath(c); err == nil {
			return path, nil
		}
	}
	for _, path := range wellKnownBrowsers() {
		if executable(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("no Chromium-family browser found (looked for %s)", strings.Join(chromeCandidates, ", "))
}

// frugalFlags cut what a browser does when nobody is looking at it. Factor
// drives one tab on machines with a couple of slow cores and a few gigabytes
// of RAM, so background networking, component updates, sync and the media
// router are pure overhead. --disable-dev-shm-usage matters most of all on
// the small distributions: their /dev/shm is tiny, and a renderer that fills
// it dies mid-page.
var frugalFlags = []chromedp.ExecAllocatorOption{
	chromedp.Flag("disable-background-networking", true),
	chromedp.Flag("disable-component-update", true),
	chromedp.Flag("disable-default-apps", true),
	chromedp.Flag("disable-sync", true),
	chromedp.Flag("disable-dev-shm-usage", true),
	chromedp.Flag("metrics-recording-only", true),
	chromedp.Flag("no-pings", true),
	chromedp.Flag("disable-features", "Translate,MediaRouter,OptimizationHints,InterestFeedContentSuggestions"),
	chromedp.Flag("renderer-process-limit", "2"),
}

// stealthJS runs before any page script and removes the tells that mark a
// browser as driven: navigator.webdriver, the empty plugin list a headless
// profile reports, the missing window.chrome object, and a Notification
// permission that answers "prompt" while the page is not focused.
//
// This is the client-side half of staying unremarkable. The other half — that
// attaching a CDP client calls Runtime.enable, which detectors watch for — is
// not fixable from here without patching chromedp itself.
const stealthJS = `
delete Object.getPrototypeOf(navigator).webdriver;
if (!window.chrome) { window.chrome = { runtime: {} }; }
if (navigator.plugins.length === 0) {
  Object.defineProperty(navigator, 'plugins', {
    get: () => [1, 2, 3].map(i => ({ name: 'Chromium PDF Plugin ' + i })),
  });
}
const query = navigator.permissions.query.bind(navigator.permissions);
navigator.permissions.query = (p) =>
  p && p.name === 'notifications'
    ? Promise.resolve({ state: Notification.permission, name: p.name, onchange: null })
    : query(p);
`

// displayAvailable reports whether this process could open a window at all.
// A visible browser is the default — watching the agent work is half the
// point — but a gateway started from an ssh shell or a service manager has no
// display to open one on, and a headful Chrome there does not degrade
// gracefully: it refuses to start. Deciding this at launch rather than at
// setup also means `factor init` over ssh does not condemn a desktop machine
// to headless browsing forever.
func displayAvailable() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func devtoolsAlive(url string) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(url + "/json/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// ensure returns a live tab context, attaching or launching on first use.
func (s *Session) ensure() (context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tabCtx != nil && s.tabCtx.Err() == nil {
		return s.tabCtx, nil
	}
	s.teardownLocked()

	base := context.Background()
	attach := s.cfg.AttachURL
	if attach == "" && devtoolsAlive("http://127.0.0.1:9222") {
		attach = "http://127.0.0.1:9222"
	}

	var allocCtx context.Context
	if attach != "" {
		allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(base, attach)
	} else {
		binary, err := FindBrowserBinary(s.cfg.Command)
		if err != nil {
			return nil, fmt.Errorf("%v — set browser.attach_url to an existing browser's DevTools port, or install one", err)
		}
		opts := []chromedp.ExecAllocatorOption{
			chromedp.ExecPath(binary),
			chromedp.NoFirstRun,
			chromedp.NoDefaultBrowserCheck,
			chromedp.Flag("disable-blink-features", "AutomationControlled"),
		}
		opts = append(opts, frugalFlags...)
		if s.cfg.UserDataDir != "" {
			if err := os.MkdirAll(s.cfg.UserDataDir, 0o755); err == nil {
				opts = append(opts, chromedp.UserDataDir(s.cfg.UserDataDir))
			}
		}
		if s.cfg.Headless || !displayAvailable() {
			opts = append(opts, chromedp.Headless, chromedp.Flag("disable-gpu", true))
		}
		// Chrome refuses to start as root, and distros that restrict
		// unprivileged user namespaces (Ubuntu 23.10+, most CI images,
		// containers) leave it with "No usable sandbox!". Dropping the
		// sandbox is the documented workaround, so it is offered as an
		// explicit opt-in rather than silently on.
		if s.cfg.NoSandbox {
			opts = append(opts, chromedp.NoSandbox)
		}
		// chromedp gives Chrome 20s to print its DevTools socket, which a
		// cold start on a loaded CI runner — or on the low-resource desktops
		// Factor targets — can miss. The outer 45s guard below still bounds
		// the whole start.
		opts = append(opts, chromedp.WSURLReadTimeout(40*time.Second))
		allocCtx, s.allocCancel = chromedp.NewExecAllocator(base, opts...)
	}

	s.tabCtx, s.tabCancel = chromedp.NewContext(allocCtx)
	// Materialize the browser now so failures surface here, not mid-action.
	// The first Run must receive the tab context itself: the browser's
	// lifetime binds to the context of that first call, so a timeout
	// wrapper here would kill the browser the moment it fired/cancelled.
	done := make(chan error, 1)
	go func() {
		done <- chromedp.Run(s.tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthJS).Do(ctx)
			return err
		}))
	}()
	select {
	case err := <-done:
		if err != nil {
			s.teardownLocked()
			return nil, fmt.Errorf("browser start failed: %w", err)
		}
	case <-time.After(45 * time.Second):
		s.teardownLocked()
		return nil, fmt.Errorf("browser start timed out")
	}
	return s.tabCtx, nil
}

// teardownLocked releases the current browser handles exactly once. Calling
// chromedp's cancel funcs twice on a half-started browser blocks forever, so
// every field is cleared as it is released. The caller holds s.mu.
func (s *Session) teardownLocked() {
	if s.tabCancel != nil {
		s.tabCancel()
		s.tabCancel = nil
	}
	if s.allocCancel != nil {
		s.allocCancel()
		s.allocCancel = nil
	}
	s.tabCtx = nil
}

// Close shuts the managed browser down.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teardownLocked()
}

// run executes chromedp actions with a per-call timeout.
func (s *Session) run(ctx context.Context, timeout time.Duration, actions ...chromedp.Action) error {
	tab, err := s.ensure()
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(tab, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- chromedp.Run(runCtx, actions...) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

// selectorFor resolves a ref (e3) or raw CSS selector.
func (s *Session) selectorFor(refOrSelector string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sel, ok := s.refs[refOrSelector]; ok {
		return sel
	}
	return refOrSelector
}

// readScript enumerates page text and interactive elements with stable
// generated selectors.
const readScript = `(() => {
  const els = document.querySelectorAll('a[href], button, input, select, textarea, [role=button], [onclick]');
  const items = [];
  let i = 0;
  const cssPath = (el) => {
    const parts = [];
    while (el && el.nodeType === 1 && parts.length < 6) {
      let part = el.tagName.toLowerCase();
      if (el.id) { parts.unshift(part + '#' + CSS.escape(el.id)); break; }
      const parent = el.parentNode;
      if (parent) {
        const idx = Array.prototype.indexOf.call(parent.children, el) + 1;
        part += ':nth-child(' + idx + ')';
      }
      parts.unshift(part);
      el = el.parentNode;
    }
    return parts.join(' > ');
  };
  for (const el of els) {
    if (i >= 60) break;
    const rect = el.getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) continue;
    i++;
    const label = (el.innerText || el.value || el.placeholder || el.getAttribute('aria-label') || el.href || '')
      .trim().replace(/\s+/g, ' ').slice(0, 80);
    items.push({ ref: 'e' + i, tag: el.tagName.toLowerCase(), type: el.type || '', label, selector: cssPath(el) });
  }
  return {
    title: document.title,
    url: location.href,
    text: (document.body ? document.body.innerText : '').replace(/\n{3,}/g, '\n\n').slice(0, 8000),
    elements: items,
  };
})()`

type pageRead struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Text     string `json:"text"`
	Elements []struct {
		Ref      string `json:"ref"`
		Tag      string `json:"tag"`
		Type     string `json:"type"`
		Label    string `json:"label"`
		Selector string `json:"selector"`
	} `json:"elements"`
}

func (s *Session) read(ctx context.Context) (*pageRead, error) {
	var result pageRead
	if err := s.run(ctx, 20*time.Second, chromedp.Evaluate(readScript, &result)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.refs = map[string]string{}
	for _, el := range result.Elements {
		s.refs[el.Ref] = el.Selector
	}
	s.mu.Unlock()
	return &result, nil
}

func formatRead(r *pageRead) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\n%s\n\nInteractive elements (use refs with browser_click / browser_fill):\n", r.Title, r.URL, r.Text)
	for _, el := range r.Elements {
		kind := el.Tag
		if el.Type != "" {
			kind += ":" + el.Type
		}
		fmt.Fprintf(&b, "  %s <%s> %q\n", el.Ref, kind, el.Label)
	}
	return b.String()
}

// Available reports whether this build carries the browser suite at all.
func Available() bool { return true }

// Verify launches the configured browser and drives one round-trip through
// it, so `factor init` can report a browser that actually works rather than
// one that merely exists on disk. Every other wizard step checks itself the
// same way.
func Verify(ctx context.Context, cfg config.BrowserConfig) error {
	s := NewSession(cfg, "")
	defer s.Close()
	var state string
	if err := s.run(ctx, 90*time.Second,
		chromedp.Navigate("about:blank"),
		chromedp.Evaluate(`document.readyState`, &state)); err != nil {
		return err
	}
	if state != "complete" {
		return fmt.Errorf("the browser started but never finished a page (readyState %q)", state)
	}
	return nil
}

// NewTools returns the browser tool suite sharing one session.
func NewTools(cfg config.BrowserConfig, workspace string) ([]tools.Tool, func()) {
	s := NewSession(cfg, workspace)
	suite := []tools.Tool{
		&navigateTool{s}, &readTool{s}, &clickTool{s}, &fillTool{s},
		&screenshotTool{s}, &evalTool{s}, &backTool{s},
	}
	if !cfg.FastPath {
		return suite, s.Close
	}
	f := newFastSession(cfg)
	return append(suite, &fetchTool{f}), func() {
		s.Close()
		f.Close()
	}
}

type navigateTool struct{ s *Session }

func (t *navigateTool) Name() string { return "browser_navigate" }
func (t *navigateTool) Description() string {
	return "Open a URL in the browser (a visible window unless configured headless), then read the page."
}
func (t *navigateTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"url": map[string]any{"type": "string", "description": "Absolute URL including the scheme, e.g. https://example.com"}},
		"required":   []any{"url"},
	}
}
func (t *navigateTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	url := tools.StringArg(args, "url")
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	if err := t.s.run(ctx, 45*time.Second, chromedp.Navigate(url), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return tools.Errorf("navigate failed: %v", err)
	}
	r, err := t.s.read(ctx)
	if err != nil {
		return tools.Errorf("page loaded but read failed: %v", err)
	}
	return tools.Text(formatRead(r))
}

type readTool struct{ s *Session }

func (t *readTool) Name() string { return "browser_read" }
func (t *readTool) Description() string {
	return "Read the current page: title, text, and interactive elements with refs."
}
func (t *readTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *readTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	r, err := t.s.read(ctx)
	if err != nil {
		return tools.Errorf("read failed: %v", err)
	}
	return tools.Text(formatRead(r))
}

type clickTool struct{ s *Session }

func (t *clickTool) Name() string { return "browser_click" }
func (t *clickTool) Description() string {
	return "Click an element by ref (from browser_read) or CSS selector, then read the resulting page."
}
func (t *clickTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"target": map[string]any{"type": "string", "description": "Ref like e3, or a CSS selector"}},
		"required":   []any{"target"},
	}
}
func (t *clickTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	sel := t.s.selectorFor(tools.StringArg(args, "target"))
	selJSON, _ := json.Marshal(sel)
	// Native element.click() beats synthesized mouse events for reliability
	// across headless modes and Chrome versions.
	script := fmt.Sprintf(`(() => {
		const el = document.querySelector(%s);
		if (!el) return "missing";
		el.scrollIntoView({block: "center"});
		el.click();
		return "ok";
	})()`, selJSON)
	var outcome string
	if err := t.s.run(ctx, 20*time.Second, chromedp.Evaluate(script, &outcome)); err != nil {
		return tools.Errorf("click %q failed: %v", sel, err)
	}
	if outcome == "missing" {
		return tools.Errorf("no element matches %q — run browser_read for fresh refs", sel)
	}
	time.Sleep(500 * time.Millisecond) // let navigations/XHR settle a beat
	r, err := t.s.read(ctx)
	if err != nil {
		return tools.Errorf("clicked, but read failed: %v", err)
	}
	return tools.Text(formatRead(r))
}

type fillTool struct{ s *Session }

func (t *fillTool) Name() string { return "browser_fill" }
func (t *fillTool) Description() string {
	return "Type text into an input by ref or CSS selector; optionally press Enter to submit."
}
func (t *fillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{"type": "string", "description": "Ref like e3 from browser_read, or a CSS selector"},
			"text":   map[string]any{"type": "string", "description": "Text to type into the field; this replaces whatever it already contains"},
			"submit": map[string]any{"type": "boolean", "description": "Press Enter after typing, submitting the form (default false)"},
		},
		"required": []any{"target", "text"},
	}
}
func (t *fillTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	sel := t.s.selectorFor(tools.StringArg(args, "target"))
	selJSON, _ := json.Marshal(sel)
	textJSON, _ := json.Marshal(tools.StringArg(args, "text"))
	submitJSON, _ := json.Marshal(tools.BoolArg(args, "submit", false))
	script := fmt.Sprintf(`(() => {
		const el = document.querySelector(%s);
		if (!el) return "missing";
		el.scrollIntoView({block: "center"});
		el.focus();
		const setter = Object.getOwnPropertyDescriptor(
			el.tagName === "TEXTAREA" ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype, "value");
		if (setter && setter.set) { setter.set.call(el, %s); } else { el.value = %s; }
		el.dispatchEvent(new Event("input", {bubbles: true}));
		el.dispatchEvent(new Event("change", {bubbles: true}));
		if (%s) {
			if (el.form && el.form.requestSubmit) { el.form.requestSubmit(); }
			else {
				const opts = {key: "Enter", code: "Enter", keyCode: 13, bubbles: true};
				el.dispatchEvent(new KeyboardEvent("keydown", opts));
				el.dispatchEvent(new KeyboardEvent("keyup", opts));
			}
		}
		return "ok";
	})()`, selJSON, textJSON, textJSON, submitJSON)
	var outcome string
	if err := t.s.run(ctx, 20*time.Second, chromedp.Evaluate(script, &outcome)); err != nil {
		return tools.Errorf("fill %q failed: %v", sel, err)
	}
	if outcome == "missing" {
		return tools.Errorf("no element matches %q — run browser_read for fresh refs", sel)
	}
	return tools.Textf("Filled %s.", sel)
}

type screenshotTool struct{ s *Session }

func (t *screenshotTool) Name() string { return "browser_screenshot" }
func (t *screenshotTool) Description() string {
	return "Capture the current page to a JPEG in the workspace and return its path."
}
func (t *screenshotTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *screenshotTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	var buf []byte
	if err := t.s.run(ctx, 30*time.Second, chromedp.FullScreenshot(&buf, 80)); err != nil {
		return tools.Errorf("screenshot failed: %v", err)
	}
	dir := filepath.Join(t.s.workspace, "screenshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.Errorf("%v", err)
	}
	// FullScreenshot encodes JPEG at any quality below 100, so the file
	// extension must say so.
	path := filepath.Join(dir, fmt.Sprintf("shot-%d.jpg", time.Now().Unix()))
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return tools.Errorf("%v", err)
	}
	return &tools.Result{ForLLM: "Saved screenshot to " + path, ForUser: path}
}

type evalTool struct{ s *Session }

func (t *evalTool) Name() string { return "browser_eval" }
func (t *evalTool) Description() string {
	return "Evaluate a JavaScript expression on the current page and return its JSON result."
}
func (t *evalTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"expression": map[string]any{"type": "string", "description": "A JavaScript expression that evaluates to a JSON-serializable value; the result is returned, not printed"}},
		"required":   []any{"expression"},
	}
}
func (t *evalTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	var result any
	if err := t.s.run(ctx, 20*time.Second, chromedp.Evaluate(tools.StringArg(args, "expression"), &result)); err != nil {
		return tools.Errorf("eval failed: %v", err)
	}
	return tools.Textf("%v", result)
}

type backTool struct{ s *Session }

func (t *backTool) Name() string        { return "browser_back" }
func (t *backTool) Description() string { return "Go back one page in browser history." }
func (t *backTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *backTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	// history.back() rather than chromedp.NavigateBack(): the latter waits
	// for a lifecycle event that a back/forward-cache restore never fires,
	// so it stalls until timeout on most real pages.
	var outcome string
	script := `(() => {
		if (history.length <= 1) return "no-history";
		history.back();
		return "ok";
	})()`
	if err := t.s.run(ctx, 20*time.Second, chromedp.Evaluate(script, &outcome)); err != nil {
		return tools.Errorf("back failed: %v", err)
	}
	if outcome == "no-history" {
		return tools.Errorf("back failed: this tab has no earlier page to return to")
	}
	time.Sleep(600 * time.Millisecond) // let the restore or navigation settle
	r, err := t.s.read(ctx)
	if err != nil {
		return tools.Errorf("went back, but read failed: %v", err)
	}
	return tools.Text(formatRead(r))
}
