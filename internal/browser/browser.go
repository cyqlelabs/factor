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
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
		// The browser the user is already logged into is the one that sees
		// the web the way they do: their cookies, their sessions, and a
		// profile with history behind it, which the sites that turn away
		// automation are far likelier to serve.
		slog.Info("browser: attached to the browser already running here", "devtools", attach)
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
		slog.Warn("browser: launching a profile of its own, which is logged in nowhere and which sites that block automation are likelier to refuse — "+
			"start your own browser with --remote-debugging-port=9222, or point browser.attach_url at one, to browse as yourself",
			"binary", binary)
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
// readTemplate renders the page for the model. The filter and limit are
// spliced in as JSON literals by readPage.
//
// Two things here are the difference between a page the agent can work and a
// page it can only describe. Elements inside the main content region come
// first, because a listing page puts its skip links, shortcuts menu and
// navigation first in the DOM — taking the first N in document order returns
// the furniture and none of the results. And the text is read from that same
// region when there is one, for the same reason.
const readTemplate = `(() => {
  const FILTER = %s, LIMIT = %s, MAX_TEXT = %s;
  const sel = 'a[href], button, input, select, textarea, [role=button], [onclick]';
  const main = document.querySelector('main, [role=main]');
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
  const labelOf = (el) =>
    (el.innerText || el.value || el.placeholder || el.getAttribute('aria-label') || el.href || '')
      .trim().replace(/\s+/g, ' ').slice(0, 80);

  const visible = [];
  for (const el of document.querySelectorAll(sel)) {
    const rect = el.getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) continue;
    visible.push(el);
  }
  // Three tiers, content first: what the page is about, then the controls it
  // offers over that content, then the site furniture wrapped around it. On a
  // listing this is the difference between the first hundred refs being skip
  // links and being results.
  const CHROME = 'nav, aside, header, footer, [role=navigation], [role=complementary], [role=banner], [role=contentinfo], [role=search]';
  const rank = (el) => {
    const inMain = main ? main.contains(el) : true;
    if (inMain && !el.closest(CHROME)) return 0;
    return inMain ? 1 : 2;
  };
  const ordered = [];
  for (const tier of [0, 1, 2]) { for (const el of visible) { if (rank(el) === tier) ordered.push(el); } }

  const needle = FILTER.toLowerCase();
  const matched = needle
    ? ordered.filter((el) => (labelOf(el) + ' ' + (el.getAttribute('href') || '')).toLowerCase().includes(needle))
    : ordered;

  const items = matched.slice(0, LIMIT).map((el, i) => ({
    ref: 'e' + (i + 1), tag: el.tagName.toLowerCase(), type: el.type || '',
    label: labelOf(el), selector: cssPath(el),
  }));

  const region = main || document.body;
  const text = (region ? region.innerText : '').replace(/\n{3,}/g, '\n\n');
  return {
    title: document.title,
    url: location.href,
    text: text.slice(0, MAX_TEXT),
    textTotal: text.length,
    fromMain: !!main,
    elements: items,
    elementTotal: ordered.length,
    matchTotal: matched.length,
  };
})()`

// maxTextChars is how much page text one read returns. pageRead.TextTotal
// carries the length before the cut, which is what lets formatRead say a page
// was truncated rather than leaving the model to assume it was short.
const maxTextChars = 8000

// Element budget for one read. The default is generous because the failure it
// replaces was silent: a page with 560 controls handing back 60 of them, with
// nothing in the output to say the other 500 existed.
const (
	defaultElementLimit = 100
	maxElementLimit     = 300
)

type pageRead struct {
	Title        string `json:"title"`
	URL          string `json:"url"`
	Text         string `json:"text"`
	TextTotal    int    `json:"textTotal"`
	FromMain     bool   `json:"fromMain"`
	ElementTotal int    `json:"elementTotal"`
	MatchTotal   int    `json:"matchTotal"`
	Elements     []struct {
		Ref      string `json:"ref"`
		Tag      string `json:"tag"`
		Type     string `json:"type"`
		Label    string `json:"label"`
		Selector string `json:"selector"`
	} `json:"elements"`
}

// read returns the whole page at the default budget, which is what every
// tool that ends by showing the user where it landed wants.
func (s *Session) read(ctx context.Context) (*pageRead, error) {
	return s.readPage(ctx, "", defaultElementLimit)
}

// elementLimit keeps a caller's budget inside what one read can usefully
// carry: an absent or nonsense limit falls back to the default, and no limit
// buys more than the page-sized ceiling.
func elementLimit(limit int) int {
	if limit <= 0 {
		return defaultElementLimit
	}
	return min(limit, maxElementLimit)
}

func (s *Session) readPage(ctx context.Context, filter string, limit int) (*pageRead, error) {
	filterJSON, _ := json.Marshal(filter)
	limitJSON, _ := json.Marshal(elementLimit(limit))

	var result pageRead
	script := fmt.Sprintf(readTemplate, filterJSON, limitJSON, strconv.Itoa(maxTextChars))
	if err := s.run(ctx, 20*time.Second, chromedp.Evaluate(script, &result)); err != nil {
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

// formatRead renders a read for the model, and says out loud what it left
// out. A truncated page the model cannot tell is truncated is the difference
// between "there are no results" and "the results are further down".
func formatRead(r *pageRead) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", r.Title, r.URL)
	if r.FromMain {
		b.WriteString("(text read from the page's main content region)\n")
	}
	if r.TextTotal > len(r.Text) {
		fmt.Fprintf(&b, "(showing %d of %d characters — browser_scroll or browser_eval can reach the rest)\n",
			len(r.Text), r.TextTotal)
	}
	fmt.Fprintf(&b, "\n%s\n\n%s\n", r.Text, elementHeader(r))
	for _, el := range r.Elements {
		kind := el.Tag
		if el.Type != "" {
			kind += ":" + el.Type
		}
		fmt.Fprintf(&b, "  %s <%s> %q\n", el.Ref, kind, el.Label)
	}
	return b.String()
}

// elementHeader states the budget in the one place the model reads it, so a
// page with more controls than fit reads as an invitation to narrow rather
// than as the whole page.
func elementHeader(r *pageRead) string {
	head := fmt.Sprintf("Interactive elements (%d shown", len(r.Elements))
	switch {
	case r.MatchTotal > len(r.Elements) && r.MatchTotal < r.ElementTotal:
		head += fmt.Sprintf(" of %d matching, %d on the page", r.MatchTotal, r.ElementTotal)
	case r.MatchTotal > len(r.Elements):
		head += fmt.Sprintf(" of %d on the page — narrow with filter, or raise limit", r.ElementTotal)
	case r.MatchTotal < r.ElementTotal:
		head += fmt.Sprintf(" of %d on the page, filtered", r.ElementTotal)
	}
	return head + "; use refs with browser_click / browser_fill):"
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
		&navigateTool{s}, &readTool{s}, &scrollTool{s}, &clickTool{s}, &fillTool{s},
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
	return "Open a URL in the browser and read the page. This is a real browser carrying the user's own session and cookies, so it sees what a plain fetch cannot: JavaScript-rendered pages, listings, logged-in areas, and anything that has to be scrolled, filled or clicked. Reach for it as soon as a fetch comes back thin rather than describing the empty shell."
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
	return "Read the current page: title, text, and interactive elements with refs to click or fill. Content inside the page's main region comes first, so results lead and site navigation follows. The output says how many elements exist and how many it showed — when it shows fewer than exist, pass filter (matches the label or href, e.g. filter='add to cart') or raise limit rather than concluding the page has nothing on it."
}
func (t *readTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filter": map[string]any{"type": "string", "description": "Only list elements whose label or link contains this text (case-insensitive)"},
			"limit":  map[string]any{"type": "integer", "description": "How many elements to list (default 100, max 300)"},
		},
	}
}
func (t *readTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	r, err := t.s.readPage(ctx, tools.StringArg(args, "filter"), tools.IntArg(args, "limit", defaultElementLimit))
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

// scrollTool exists because a listing page loads as you travel down it: the
// results past the first screen are not in the DOM until something scrolls,
// so a page read at the top is a page with most of its content missing.
type scrollTool struct{ s *Session }

func (t *scrollTool) Name() string { return "browser_scroll" }
func (t *scrollTool) Description() string {
	return "Scroll the page and read where it lands. Use it to reach results that load as you go: to='bottom' jumps to the end (repeat it to pull in more of an infinite list), to='down' or 'up' moves one screen, to='top' returns. Lazily loaded content only exists after the scroll that reveals it."
}
func (t *scrollTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"to":     map[string]any{"type": "string", "enum": []any{"down", "up", "bottom", "top"}, "description": "Where to scroll (default down, one screen)"},
			"filter": map[string]any{"type": "string", "description": "Passed to the read that follows, same meaning as browser_read's"},
		},
	}
}
func (t *scrollTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	to := tools.StringArg(args, "to")
	var move string
	switch to {
	case "top":
		move = "window.scrollTo(0, 0)"
	case "up":
		move = "window.scrollBy(0, -window.innerHeight * 0.9)"
	case "bottom":
		move = "window.scrollTo(0, document.body.scrollHeight)"
	default:
		to, move = "down", "window.scrollBy(0, window.innerHeight * 0.9)"
	}
	// The height before and after says whether the scroll actually pulled
	// anything in, which is what "repeat it" needs to be answerable.
	script := fmt.Sprintf(`(() => { const before = document.body.scrollHeight; %s; return before; })()`, move)
	var before float64
	if err := t.s.run(ctx, 20*time.Second, chromedp.Evaluate(script, &before)); err != nil {
		return tools.Errorf("scroll failed: %v", err)
	}
	time.Sleep(900 * time.Millisecond) // let lazily loaded content arrive
	var after float64
	if err := t.s.run(ctx, 20*time.Second, chromedp.Evaluate(`document.body.scrollHeight`, &after)); err != nil {
		return tools.Errorf("scrolled, but the page could not be measured: %v", err)
	}
	r, err := t.s.readPage(ctx, tools.StringArg(args, "filter"), defaultElementLimit)
	if err != nil {
		return tools.Errorf("scrolled, but read failed: %v", err)
	}
	grew := ""
	if after > before {
		grew = fmt.Sprintf("Scrolling %s loaded more of the page (it grew from %.0f to %.0f pixels tall).\n", to, before, after)
	}
	return tools.Text(grew + formatRead(r))
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
	// The back is queued rather than called inline: navigating tears down the
	// execution context this very Evaluate is waiting on a reply from, so
	// calling it directly races its own result and surfaces the navigation as
	// "Inspected target navigated or closed" on a back that actually worked.
	script := `(() => {
		if (history.length <= 1) return "no-history";
		setTimeout(() => history.back(), 0);
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
