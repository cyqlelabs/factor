//go:build !nobrowser

// Package browser gives the agent a real browser via the Chrome DevTools
// Protocol (chromedp): it attaches to the user's running Chrome/Chromium/
// Brave when a DevTools port is open, otherwise launches a managed instance
// — visible by default, so the user can watch the agent work. When the
// machine has no Chromium-family browser at all, one is provisioned on the
// spot (see install.go): `factor init` does it up front, and a session that
// finds none does it rather than reporting the suite unavailable. Build with
// -tags nobrowser to strip the whole suite from the binary.
package browser

import (
	"context"
	"encoding/json"
	"errors"
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
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Session lazily owns one browser connection shared by all browser tools.
//
// The connection and the tabs on it are deliberately separate lifetimes.
// browserCtx is a bare connection that never owns a tab, so releasing it
// closes nothing; every tab hangs off it as its own context. That split is
// what lets the agent move between the user's tabs at all: chromedp closes a
// tab when its context is cancelled, so a session that kept one tab context
// and re-made it on every switch would close the user's tabs as it went.
type Session struct {
	cfg       config.BrowserConfig
	workspace string
	guard     *tools.PathGuard

	mu          sync.Mutex
	allocCtx    context.Context
	allocCancel context.CancelFunc
	browserCtx  context.Context
	browserStop context.CancelFunc
	attached    bool // driving the user's own browser rather than one Factor launched
	tabs        map[target.ID]*tabHandle
	cur         *tabHandle
	tabRefs     map[int]target.ID // tab number from the last listing -> target
	refs        map[string]string // eN -> CSS selector from the last read
}

func NewSession(cfg config.BrowserConfig, workspace string, guard *tools.PathGuard) *Session {
	return &Session{
		cfg:       cfg,
		workspace: workspace,
		guard:     guard,
		tabs:      map[target.ID]*tabHandle{},
		tabRefs:   map[int]target.ID{},
		refs:      map[string]string{},
	}
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

// devtoolsProbe is where a browser the user is already running is expected to
// be listening. It is a variable so the live tests can point it at a dead
// port: a test that attached to the developer's own browser would open and
// close tabs in it, which is neither a fixture nor a fair test.
var devtoolsProbe = "http://127.0.0.1:9222"

// provisionEngine is EnsureEngine behind a seam, so a test can exercise the
// provisioning path — and a CI box without a browser can be stopped from
// downloading one — without reaching the network.
var provisionEngine = EnsureEngine

func devtoolsAlive(url string) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(url + "/json/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// ensureBrowser returns a live browser connection, attaching or launching on
// first use, without claiming a tab. Asking the browser what it has open must
// not add a tab to the answer: the tools that only need the connection —
// listing, switching, waiting for a close — used to open one of Factor's own
// as a side effect, so closing that stray tab re-created it on the very next
// listing and the user watched a blank tab come back however often they shut
// it.
//
// Deciding what to launch happens outside the lock because it can mean
// downloading a browser, and a session that held its mutex through a hundred
// megabytes would stall its own shutdown behind the download.
func (s *Session) ensureBrowser(ctx context.Context) (context.Context, error) {
	s.mu.Lock()
	if s.browserCtx != nil && s.browserCtx.Err() == nil {
		defer s.mu.Unlock()
		return s.browserCtx, nil
	}
	s.mu.Unlock()

	attach := s.cfg.AttachURL
	var binary string
	if attach == "" && devtoolsAlive(devtoolsProbe) {
		attach = devtoolsProbe
	}
	if attach == "" {
		var err error
		if binary, err = s.ensureBinary(ctx); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Another caller may have connected while the lock was open.
	if s.browserCtx == nil || s.browserCtx.Err() != nil {
		if err := s.connectLocked(attach, binary); err != nil {
			return nil, err
		}
	}
	return s.browserCtx, nil
}

// ensure returns a live tab context, connecting and claiming a tab on first
// use. This is the entry point for everything that acts on a page; anything
// that only asks the browser a question wants ensureBrowser instead.
func (s *Session) ensure(ctx context.Context) (context.Context, error) {
	s.mu.Lock()
	if s.cur != nil && s.cur.ctx.Err() == nil {
		defer s.mu.Unlock()
		return s.cur.ctx, nil
	}
	s.mu.Unlock()

	if _, err := s.ensureBrowser(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.ctx.Err() == nil {
		return s.cur.ctx, nil
	}
	h, err := s.openTabLocked()
	if err != nil {
		return nil, err
	}
	s.cur = h
	return h.ctx, nil
}

// ensureBinary resolves the browser to launch, provisioning one when the
// machine has none. Handing the user a command to run instead is not an
// option: on the boxes that most need this — a stripped distribution with no
// package manager Factor could call anyway — that answer never becomes a
// working browser, and the suite would stay dead for the life of the install.
func (s *Session) ensureBinary(ctx context.Context) (string, error) {
	path, err := FindBrowserBinary(s.cfg.Command)
	if err == nil {
		return path, nil
	}
	if s.cfg.Command != "" {
		// A configured browser that is not there is a mistake to report, not
		// something to quietly substitute a different browser for.
		return "", fmt.Errorf("the configured browser %q was not found (%w) — correct browser.command, or set browser.attach_url to an existing browser's DevTools port", s.cfg.Command, err)
	}
	slog.Info("browser: no browser on this machine, provisioning one")
	installCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	path, installed, err := provisionEngine(installCtx, config.Home(), func(format string, args ...any) {
		slog.Info("browser: " + fmt.Sprintf(format, args...))
	})
	if err != nil {
		return "", fmt.Errorf("no browser is installed and provisioning one failed: %w", err)
	}
	if installed {
		slog.Info("browser: provisioned", "binary", path)
	}
	return path, nil
}

// connectLocked opens the browser connection without claiming a tab on it.
// Materializing through Targets rather than an ordinary action is the point:
// a chromedp context that has never run a page action owns no target, so
// releasing this one later closes nothing the user was looking at.
func (s *Session) connectLocked(attach, binary string) error {
	s.teardownLocked()

	base := context.Background()
	s.attached = attach != ""

	var allocCtx context.Context
	if attach != "" {
		// The browser the user is already logged into is the one that sees
		// the web the way they do: their cookies, their sessions, and a
		// profile with history behind it, which the sites that turn away
		// automation are far likelier to serve.
		slog.Info("browser: attached to the browser already running here", "devtools", attach)
		allocCtx, s.allocCancel = chromedp.NewRemoteAllocator(base, attach)
	} else {
		if binary == "" {
			return fmt.Errorf("no browser to launch and none could be provisioned")
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

	s.allocCtx = allocCtx
	s.browserCtx, s.browserStop = chromedp.NewContext(allocCtx)
	// Materialize the browser now so failures surface here, not mid-action.
	// The first call must receive the browser context itself: the browser's
	// lifetime binds to the context of that first call, so a timeout wrapper
	// here would kill the browser the moment it fired/cancelled.
	done := make(chan error, 1)
	go func() {
		_, err := chromedp.Targets(s.browserCtx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			s.teardownLocked()
			return fmt.Errorf("browser start failed: %w", err)
		}
	case <-time.After(45 * time.Second):
		s.teardownLocked()
		return fmt.Errorf("browser start timed out")
	}
	return nil
}

// openTabLocked puts a tab of Factor's own on the connection. On the user's
// own browser this is always a fresh tab: adopting whatever they happened to
// be reading would mean typing into it. On a browser Factor launched, the
// blank tab it came up with is adopted instead, so the window does not end up
// with a stray empty tab beside the one being driven.
func (s *Session) openTabLocked() (*tabHandle, error) {
	if !s.attached && len(s.tabs) == 0 {
		// A launched browser's first blank tab can lag the connection on a
		// slow cold start, and racing it costs more than tidiness: a session
		// that opens its own tab beside the one still materializing strands a
		// blank tab that inflates every tab count from then on — including
		// the guard that refuses to close the only open tab, which then lets
		// the only real tab be closed. Wait for the tab rather than race it.
		deadline := time.Now().Add(3 * time.Second)
		for {
			if infos, err := chromedp.Targets(s.browserCtx); err == nil {
				var pages []*target.Info
				for _, t := range infos {
					if interesting(t) {
						pages = append(pages, t)
					}
				}
				if len(pages) == 1 {
					return s.attachLocked(pages[0].TargetID, true)
				}
				if len(pages) > 1 {
					break // a browser already carrying tabs: open one of our own
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	id, err := s.createTabLocked()
	if err != nil {
		return nil, err
	}
	return s.attachLocked(id, true)
}

// createTabLocked opens a tab on the connection and returns its target.
//
// chromedp creates every target of its own with newWindow: true, which on the
// browser the user is already working in means each tab Factor opens is a
// separate window landing on top of theirs — the whole point of attaching to
// their browser is to be somewhere they already are. The target is created
// here with the flag left off, so it opens as a tab in the window they are
// looking at, and the tab context is bound to it afterwards by id.
func (s *Session) createTabLocked() (target.ID, error) {
	c := chromedp.FromContext(s.browserCtx)
	if c == nil || c.Browser == nil {
		return "", fmt.Errorf("no browser connection")
	}
	ctx, cancel := context.WithTimeout(s.browserCtx, 45*time.Second)
	defer cancel()
	return target.CreateTarget("about:blank").Do(cdp.WithExecutor(ctx, c.Browser))
}

// attachLocked binds a tab that already exists. owned says whether closing it
// later is Factor's business: a tab it opened may be closed with its context,
// one it merely borrowed may not.
func (s *Session) attachLocked(id target.ID, owned bool) (*tabHandle, error) {
	parent := s.browserCtx
	if !owned {
		// Severed from the connection on purpose: cancelling a chromedp
		// context closes its tab, and this tab is the user's. Shutting this
		// session down must not take their tab with it.
		parent = context.WithoutCancel(s.browserCtx)
	}
	tabCtx, cancel := chromedp.NewContext(parent, chromedp.WithTargetID(id))
	h := &tabHandle{ctx: tabCtx, cancel: cancel, id: id, owned: owned}
	if err := s.materialize(h); err != nil {
		cancel()
		return nil, err
	}
	return h, nil
}

// materialize attaches the tab and installs the stealth script on it, then
// records which target it landed on.
func (s *Session) materialize(h *tabHandle) error {
	done := make(chan error, 1)
	go func() {
		done <- chromedp.Run(h.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthJS).Do(ctx)
			return err
		}))
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("attaching to the tab failed: %w", err)
		}
	case <-time.After(45 * time.Second):
		return fmt.Errorf("attaching to the tab timed out")
	}
	if c := chromedp.FromContext(h.ctx); c != nil && c.Target != nil {
		h.id = c.Target.TargetID
	}
	if h.id != "" {
		s.tabs[h.id] = h
	}
	return nil
}

// openTab opens a fresh tab and makes it current. A session that has no tab
// yet gets its first one here rather than a second one beside it: on a
// browser Factor launched, that means adopting the blank tab it came up with.
func (s *Session) openTab(ctx context.Context) error {
	if _, err := s.ensureBrowser(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openTabLocked()
	if err != nil {
		return err
	}
	s.cur = h
	s.refs = map[string]string{}
	return nil
}

// teardownLocked releases the current browser handles exactly once. Calling
// chromedp's cancel funcs twice on a half-started browser blocks forever, so
// every field is cleared as it is released. The caller holds s.mu.
func (s *Session) teardownLocked() {
	// Only tabs Factor opened are cancelled: cancelling an adopted one would
	// close a tab of the user's, so those are released by letting go of them
	// and leaving the tab exactly as it was found.
	for id, h := range s.tabs {
		if h.owned {
			h.cancel()
		}
		delete(s.tabs, id)
	}
	s.cur = nil
	if s.browserStop != nil {
		s.browserStop()
		s.browserStop = nil
	}
	if s.allocCancel != nil {
		s.allocCancel()
		s.allocCancel = nil
	}
	s.browserCtx, s.allocCtx = nil, nil
}

// Close shuts the managed browser down.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teardownLocked()
}

// run executes chromedp actions with a per-call timeout.
func (s *Session) run(ctx context.Context, timeout time.Duration, actions ...chromedp.Action) error {
	tab, err := s.ensure(ctx)
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
	// OtherTabs is filled in after the page itself is read, from the browser
	// rather than the page. It is the difference between an agent that knows
	// the user already has the right account open in the next tab and one
	// that can only find out by looking at the screen.
	OtherTabs    []string
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
	result.OtherTabs = s.otherTabs(ctx)
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
	if len(r.OtherTabs) > 0 {
		fmt.Fprintf(&b, "\nAlso open in this browser (browser_tabs to switch):\n%s\n", strings.Join(r.OtherTabs, "\n"))
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
	s := NewSession(cfg, "", nil)
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
func NewTools(cfg config.BrowserConfig, workspace string, guard *tools.PathGuard) ([]tools.Tool, func()) {
	s := NewSession(cfg, workspace, guard)
	suite := []tools.Tool{
		&navigateTool{s}, &readTool{s}, &scrollTool{s}, &clickTool{s}, &fillTool{s},
		&screenshotTool{s}, &evalTool{s}, &backTool{s},
		&tabsTool{s}, &uploadTool{s}, &keysTool{s},
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
		"properties": map[string]any{"url": map[string]any{"type": "string", "description": "Absolute URL including the scheme, e.g. https://example.com, or a local file as file:///path/to/page.html"}},
		"required":   []any{"url"},
	}
}

// navigateWait bounds how long a navigation may spend reaching the page's
// load event. A var so the live tests can shrink it: proving the salvage below
// works must not cost a test 45 real seconds of waiting.
var navigateWait = 45 * time.Second

// localOrRemote resolves what the model passed into something the browser can
// load. Everything that is not already a URL used to have https:// pinned to
// the front of it, which turned a local file into a hostname that resolves
// nowhere — and the model read that DNS failure back as "this browser cannot
// open local files" and told the user so. A file is a read like any other, so
// the same guard the upload tool applies decides whether it may be opened.
func (t *navigateTool) localOrRemote(url string) (string, error) {
	path := ""
	switch {
	case strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "https://"):
		return url, nil
	case strings.HasPrefix(url, "file://"):
		path = strings.TrimPrefix(url, "file://")
	case strings.HasPrefix(url, "/"):
		path = url // a bare absolute path is a file, never a host
	default:
		return "https://" + url, nil
	}
	if t.s.guard == nil {
		return "file://" + path, nil
	}
	resolved, err := t.s.guard.CheckRead(path)
	if err != nil {
		return "", err
	}
	return "file://" + resolved, nil
}

func (t *navigateTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	url, err := t.localOrRemote(tools.StringArg(args, "url"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	// chromedp's Navigate refuses to return before the page's load event, and
	// that event waits on every script and image: a heavy page on a slow
	// machine spends tens of seconds past DOM-ready in assets, and one with a
	// stalled asset never fires it at all. committed records that the main
	// frame really navigated, which is what separates "the site never
	// answered" from "the page is there but still loading" once the wait runs
	// out — the second is a page to read and label, not a failure to report.
	var committed atomic.Bool
	err = t.s.run(ctx, navigateWait,
		chromedp.ActionFunc(func(c context.Context) error {
			chromedp.ListenTarget(c, func(ev any) {
				if fn, ok := ev.(*page.EventFrameNavigated); ok && fn.Frame != nil && fn.Frame.ParentID == "" {
					committed.Store(true)
				}
			})
			return nil
		}),
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
	stillLoading := err != nil && committed.Load() && errors.Is(err, context.DeadlineExceeded)
	if err != nil && !stillLoading {
		return tools.Errorf("navigate failed: %v", err)
	}
	r, rerr := t.s.read(ctx)
	if rerr != nil {
		if err != nil {
			return tools.Errorf("navigate failed: %v", err)
		}
		return tools.Errorf("page loaded but read failed: %v", rerr)
	}
	out := formatRead(r)
	if stillLoading {
		out = "The page was still loading when it was read — parts of it may not have arrived yet; browser_read again once it settles.\n\n" + out
	}
	return tools.Text(out)
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
	return "Type text into a field by ref or CSS selector — a text input, a textarea, a dropdown, or a rich editor like an email body. The text is typed through the browser rather than assigned, so fields that only react to real typing (address chips, autocomplete suggestions, editors) behave as they do for a person. Optionally press Enter to submit."
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

// focusScript prepares a field to be typed into and reports what kind of
// field it turned out to be. Clearing happens by selecting what is there, so
// the typing that follows replaces it the way it would for a person: a
// dropdown and a rich text editor have no .value to assign, and an address
// field that builds chips as you type ignores one that is.
const focusScript = `(() => {
  const el = document.querySelector(%s);
  if (!el) return {kind: "missing"};
  el.scrollIntoView({block: "center"});
  const tag = el.tagName.toLowerCase();
  if (tag === "select") return {kind: "select"};
  el.focus();
  if (el.isContentEditable) {
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    if (%s) { document.execCommand("delete"); }
    return {kind: "editable"};
  }
  if (typeof el.select === "function") { el.select(); }
  if (%s) {
    const proto = tag === "textarea" ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(proto, "value");
    if (setter && setter.set) { setter.set.call(el, ""); } else { el.value = ""; }
    el.dispatchEvent(new Event("input", {bubbles: true}));
    el.dispatchEvent(new Event("change", {bubbles: true}));
  }
  return {kind: "field"};
})()`

// selectScript picks a dropdown option by value or by the text a person reads.
const selectScript = `(() => {
  const el = document.querySelector(%s), want = %s;
  if (!el) return {kind: "missing"};
  const options = Array.from(el.options || []);
  const match = options.find((o) => o.value === want) ||
    options.find((o) => o.text.trim().toLowerCase() === want.trim().toLowerCase()) ||
    options.find((o) => o.text.toLowerCase().includes(want.trim().toLowerCase()));
  if (!match) return {kind: "no-option", options: options.slice(0, 20).map((o) => o.text.trim())};
  el.value = match.value;
  el.dispatchEvent(new Event("input", {bubbles: true}));
  el.dispatchEvent(new Event("change", {bubbles: true}));
  return {kind: "select", chosen: match.text.trim()};
})()`

// readBackScript reports what the field ended up holding, which is the only
// way to notice that a page reformatted, truncated or ignored the input.
const readBackScript = `(() => {
  const el = document.querySelector(%s);
  if (!el) return "";
  return (el.isContentEditable ? el.innerText : el.value) || "";
})()`

type fillOutcome struct {
	Kind    string   `json:"kind"`
	Chosen  string   `json:"chosen"`
	Options []string `json:"options"`
}

func (t *fillTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	sel := t.s.selectorFor(tools.StringArg(args, "target"))
	text := tools.StringArg(args, "text")
	selJSON, _ := json.Marshal(sel)
	textJSON, _ := json.Marshal(text)
	clearJSON, _ := json.Marshal(text == "")

	var out fillOutcome
	if err := t.s.run(ctx, 20*time.Second,
		chromedp.Evaluate(fmt.Sprintf(focusScript, selJSON, clearJSON, clearJSON), &out)); err != nil {
		return tools.Errorf("fill %q failed: %v", sel, err)
	}
	switch out.Kind {
	case "missing":
		return tools.Errorf("no element matches %q — run browser_read for fresh refs", sel)
	case "select":
		if err := t.s.run(ctx, 20*time.Second,
			chromedp.Evaluate(fmt.Sprintf(selectScript, selJSON, textJSON), &out)); err != nil {
			return tools.Errorf("selecting in %q failed: %v", sel, err)
		}
		if out.Kind == "no-option" {
			return tools.Errorf("no option in %s matches %q; it offers: %s", sel, text, strings.Join(out.Options, ", "))
		}
		return tools.Textf("Selected %q in %s.", out.Chosen, sel)
	}

	// Typed over the protocol rather than assigned: this goes through the
	// browser's own editing path, so every event a page waits for fires in the
	// order it expects — which is what a contenteditable body needs, and what
	// turns a typed address into a recipient chip.
	if text != "" {
		if err := t.s.run(ctx, 20*time.Second, chromedp.ActionFunc(func(c context.Context) error {
			return input.InsertText(text).Do(c)
		})); err != nil {
			return tools.Errorf("typing into %q failed: %v", sel, err)
		}
	}

	var actual string
	if err := t.s.run(ctx, 15*time.Second,
		chromedp.Evaluate(fmt.Sprintf(readBackScript, selJSON), &actual)); err != nil {
		actual = text // reading back is a courtesy; a failure here is not a failed fill
	}

	if tools.BoolArg(args, "submit", false) {
		if err := t.s.run(ctx, 20*time.Second, chromedp.ActionFunc(func(c context.Context) error {
			return pressChord(c, 0, "Enter", "Enter", 13)
		})); err != nil {
			return tools.Errorf("typed into %s, but submitting failed: %v", sel, err)
		}
		time.Sleep(700 * time.Millisecond)
	}

	msg := fmt.Sprintf("Filled %s with %q.", sel, text)
	if strings.TrimSpace(actual) != strings.TrimSpace(text) {
		// Not an error: a chip field empties itself once it has the address,
		// and a formatter rewrites what it was given. Saying so lets the model
		// judge, rather than reporting a success it cannot see.
		msg += fmt.Sprintf(" The field now reads %q — the page may have reformatted, consumed or rejected it; check before continuing.", actual)
	}
	return tools.Text(msg)
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
