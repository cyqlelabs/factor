//go:build !nobrowser

package browser

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/cyqlelabs/factor/internal/tools"
)

// composePage is the shape of the thing that started all this: a compose form
// whose body is a contenteditable rather than a textarea, whose attach control
// is a styled button in front of a hidden file input, and which sends on
// Ctrl+Enter. Every one of those is a place the old suite could not reach.
const composePage = `<html><head><title>Compose</title></head><body>
<main>
  <input id="to" type="text" placeholder="Recipients">
  <select id="priority"><option value="lo">Low priority</option><option value="hi">High priority</option></select>
  <div id="body" contenteditable="true">draft text</div>
  <button id="attach" onclick="document.getElementById('file').click()">Attach files</button>
  <input id="file" type="file" style="display:none">
  <div id="status">nothing yet</div>
  <div id="attached"></div>
</main>
<script>
  document.getElementById('file').addEventListener('change', (e) => {
    document.getElementById('attached').textContent =
      'attached: ' + Array.from(e.target.files).map(f => f.name + ' (' + f.size + ' bytes)').join(', ');
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.ctrlKey) { document.getElementById('status').textContent = 'sent'; }
  });
  document.getElementById('to').addEventListener('input', (e) => {
    // A recipient field that rewrites what it is given, like the real ones do.
    if (e.target.value.includes('@')) { document.getElementById('status').textContent = 'recipient ok'; }
  });
</script>
</body></html>`

func serveCompose(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/second" {
			fmt.Fprint(w, `<html><head><title>Second Tab</title></head><body>second tab body</body></html>`)
			return
		}
		fmt.Fprint(w, composePage)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustRun(t *testing.T, tool tools.Tool, args map[string]any) string {
	t.Helper()
	res := tool.Execute(context.Background(), args)
	if res.IsError {
		t.Fatalf("%s: %s", tool.Name(), res.ForLLM)
	}
	return res.ForLLM
}

func TestFillTypesIntoContentEditableAndSelect(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	byName := liveSuite(t)
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})

	// A contenteditable has no .value to assign: the old fill silently did
	// nothing here, which is how an email body stayed empty.
	mustRun(t, byName["browser_fill"], map[string]any{"target": "#body", "text": "hello from factor"})
	out := mustRun(t, byName["browser_read"], nil)
	if !strings.Contains(out, "hello from factor") {
		t.Errorf("contenteditable was not typed into:\n%s", out)
	}
	if strings.Contains(out, "draft text") {
		t.Errorf("previous content was not replaced:\n%s", out)
	}

	// A real input must fire the events the page listens for, or a recipient
	// never becomes a recipient.
	mustRun(t, byName["browser_fill"], map[string]any{"target": "#to", "text": "nico@example.com"})
	out = mustRun(t, byName["browser_read"], nil)
	if !strings.Contains(out, "recipient ok") {
		t.Errorf("input events did not reach the page:\n%s", out)
	}

	res := byName["browser_fill"].Execute(context.Background(), map[string]any{"target": "#priority", "text": "High priority"})
	if res.IsError || !strings.Contains(res.ForLLM, "High priority") {
		t.Errorf("select by visible text: %s", res.ForLLM)
	}
	if res := byName["browser_fill"].Execute(context.Background(),
		map[string]any{"target": "#priority", "text": "nonexistent"}); !res.IsError {
		t.Error("an option that does not exist was reported as selected")
	}
}

func TestUploadAttachesFileBehindHiddenInput(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	ws := t.TempDir()
	suite, closeFn := NewTools(liveConfig(), ws, tools.NewPathGuard(ws, true, false, nil))
	t.Cleanup(closeFn)
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}

	doc := filepath.Join(ws, "report.html")
	if err := os.WriteFile(doc, []byte("<html>report</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})

	// target names the visible button; the file has to reach the hidden input
	// behind it, which is the whole difficulty.
	out := mustRun(t, byName["browser_upload"], map[string]any{"path": doc, "target": "#attach"})
	if !strings.Contains(out, "report.html (19 bytes)") {
		t.Errorf("the page never saw the file:\n%s", out)
	}
}

func TestKeysSendsShortcutToThePage(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	byName := liveSuite(t)
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})

	out := mustRun(t, byName["browser_keys"], map[string]any{"keys": "Control+Enter"})
	if !strings.Contains(out, "sent") {
		t.Errorf("the shortcut did not reach the page:\n%s", out)
	}
}

func TestTabsListSwitchAndClose(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	byName := liveSuite(t)
	ctx := context.Background()
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})

	mustRun(t, byName["browser_tabs"], map[string]any{"action": "open", "url": srv.URL + "/second"})
	out := mustRun(t, byName["browser_tabs"], map[string]any{"action": "list"})
	if !strings.Contains(out, "Compose") || !strings.Contains(out, "Second Tab") {
		t.Fatalf("both tabs should be listed:\n%s", out)
	}

	// Switching by text is what makes this usable when the numbering moves.
	out = mustRun(t, byName["browser_tabs"], map[string]any{"action": "switch", "target": "Compose"})
	if !strings.Contains(out, "Compose") {
		t.Errorf("did not land on the compose tab:\n%s", out)
	}
	// And the tab left behind must be visible from the page that is current,
	// which is the line that tells the agent the user's own tab exists at all.
	if !strings.Contains(out, "Also open in this browser") || !strings.Contains(out, "Second Tab") {
		t.Errorf("read does not mention the other tab:\n%s", out)
	}

	out = mustRun(t, byName["browser_tabs"], map[string]any{"action": "close", "target": "Second Tab"})
	if !strings.Contains(out, "Closed tab") {
		t.Errorf("close result: %s", out)
	}
	out = mustRun(t, byName["browser_tabs"], map[string]any{"action": "list"})
	if strings.Contains(out, "Second Tab") {
		t.Errorf("closed tab still listed:\n%s", out)
	}

	// The last tab is the one being driven; closing it would leave the suite
	// with nothing to act on.
	if res := byName["browser_tabs"].Execute(ctx, map[string]any{"action": "close", "target": "Compose"}); !res.IsError {
		t.Error("closing the only tab was allowed")
	}
}

// TestAdoptedTabSurvivesSessionClose is the guarantee that matters most in
// this file: chromedp closes a tab when its context is cancelled, so a session
// that adopted the user's tab and then shut down would take the tab with it.
func TestAdoptedTabSurvivesSessionClose(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	ws := t.TempDir()
	s := NewSession(liveConfig(), ws, nil)
	ctx := context.Background()

	// Claim a tab, then open a second one beside it, so there is a tab to
	// adopt the way a switch does.
	if _, err := s.readPage(ctx, "", 10); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := s.openTab(ctx); err != nil {
		t.Fatalf("open tab: %v", err)
	}
	tabs, err := s.listTabs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tabs) != 2 {
		t.Fatalf("want two open tabs, got %d: %+v", len(tabs), tabs)
	}
	id := tabs[0].ID
	if tabs[0].Current {
		id = tabs[1].ID
	}
	s.mu.Lock()
	h, err := s.attachLocked(id, false)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if h.owned {
		t.Fatal("an adopted tab must not be marked owned")
	}

	s.Close()
	if h.ctx.Err() != nil {
		t.Error("closing the session cancelled the adopted tab's context, which closes the user's tab")
	}
	_ = srv
}

// Naming a tab that is not open, or naming nothing at all, has to come back
// as an answer the model can act on — the tools it would reach for next are
// a fresh listing and a different name, and a bare protocol error says
// neither.
func TestTabsRejectsTargetsThatMatchNoOpenTab(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	byName := liveSuite(t)
	ctx := context.Background()
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})

	for _, tc := range []struct {
		name   string
		args   map[string]any
		wantIn string
	}{
		{"switch to a title nothing carries", map[string]any{"action": "switch", "target": "invoices"}, `no open tab matches "invoices"`},
		{"switch to a number nothing has", map[string]any{"action": "switch", "target": "9"}, "no tab 9 is open"},
		{"switch with no target at all", map[string]any{"action": "switch"}, "which tab?"},
		{"close a title nothing carries", map[string]any{"action": "close", "target": "invoices"}, "refusing to close the only open tab"},
	} {
		res := byName["browser_tabs"].Execute(ctx, tc.args)
		if !res.IsError {
			t.Errorf("%s: reported success (%q)", tc.name, res.ForLLM)
			continue
		}
		if !strings.Contains(res.ForLLM, tc.wantIn) {
			t.Errorf("%s: error %q does not mention %q", tc.name, res.ForLLM, tc.wantIn)
		}
	}
}

// Opening a tab with no URL is a blank tab, not a failure — and the session
// must be driving it afterwards, or the next tool would act on the old page.
func TestTabsOpenWithoutAURLLandsOnABlankTab(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	byName := liveSuite(t)
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})

	if out := mustRun(t, byName["browser_tabs"], map[string]any{"action": "open"}); !strings.Contains(out, "Opened a new tab") {
		t.Errorf("open result: %s", out)
	}
	if out := mustRun(t, byName["browser_read"], nil); !strings.Contains(out, "about:blank") {
		t.Errorf("the session did not follow the tab it opened:\n%s", out)
	}
	// The page left behind is still there, and still reachable by name.
	if out := mustRun(t, byName["browser_tabs"], map[string]any{"action": "switch", "target": "Compose"}); !strings.Contains(out, "Compose") {
		t.Errorf("could not switch back to the original tab:\n%s", out)
	}
}

// TestListingTabsDoesNotOpenOne guards the blank tab the user could not get
// rid of. Asking the browser what it has open used to claim a tab of Factor's
// own to ask with, so closing that tab re-created it — and the close itself,
// which waits for the target to disappear by listing, re-created it before it
// even returned. From the user's side a blank tab reappeared however often
// they shut it, and Factor could not say why.
func TestListingTabsDoesNotOpenOne(t *testing.T) {
	requireBrowser(t)
	srv := serveCompose(t)
	byName := liveSuite(t)
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})
	mustRun(t, byName["browser_tabs"], map[string]any{"action": "open", "url": srv.URL + "/second"})

	// Closing the tab being driven leaves the session with no tab of its own,
	// which is exactly when a listing used to conjure one.
	out := mustRun(t, byName["browser_tabs"], map[string]any{"action": "close", "target": "Second Tab"})
	if !strings.Contains(out, "Closed tab") {
		t.Fatalf("close result: %s", out)
	}
	out = mustRun(t, byName["browser_tabs"], map[string]any{"action": "list"})
	if !strings.HasPrefix(out, "1 open tabs") {
		t.Errorf("a tab was opened to answer the listing:\n%s", out)
	}
	if strings.Contains(out, "about:blank") {
		t.Errorf("a blank tab was opened to answer the listing:\n%s", out)
	}
}

// windowOf asks the browser which window a target lives in — the difference
// between a tab and a window, which the target list itself does not show.
func windowOf(t *testing.T, s *Session, id target.ID) cdpbrowser.WindowID {
	t.Helper()
	s.mu.Lock()
	bctx := s.browserCtx
	s.mu.Unlock()
	c := chromedp.FromContext(bctx)
	if c == nil || c.Browser == nil {
		t.Fatal("session has no browser connection")
	}
	win, _, err := cdpbrowser.GetWindowForTarget().WithTargetID(id).Do(cdp.WithExecutor(bctx, c.Browser))
	if err != nil {
		t.Fatalf("window for %s: %v", id, err)
	}
	return win
}

// TestTabsOpenInTheWindowAlreadyOpen is the difference between attaching to
// the user's browser and interrupting it. chromedp creates every target of
// its own with newWindow: true, so each tab Factor opened arrived as a bare
// window on top of whatever the user was doing — and closing it left them
// hunting an empty window they never asked for.
func TestTabsOpenInTheWindowAlreadyOpen(t *testing.T) {
	requireBrowser(t)
	s := NewSession(liveConfig(), t.TempDir(), nil)
	t.Cleanup(s.Close)
	ctx := context.Background()

	if err := s.openTab(ctx); err != nil {
		t.Fatalf("first tab: %v", err)
	}
	first := s.currentID()
	if err := s.openTab(ctx); err != nil {
		t.Fatalf("second tab: %v", err)
	}
	second := s.currentID()
	if first == "" || second == "" || first == second {
		t.Fatalf("want two distinct tabs, got %q and %q", first, second)
	}
	if a, b := windowOf(t, s, first), windowOf(t, s, second); a != b {
		t.Errorf("the second tab opened in a window of its own (%d, not %d)", b, a)
	}
}

// TestNavigateOpensALocalFile covers the report the agent writes and then has
// to show. Every URL that was not already http used to get https:// pinned to
// the front of it, so a file:// path became a hostname that resolves nowhere
// — and the model reported that back as the browser being unable to open
// local files at all.
func TestNavigateOpensALocalFile(t *testing.T) {
	requireBrowser(t)
	ws := t.TempDir()
	report := filepath.Join(ws, "report.html")
	if err := os.WriteFile(report, []byte(`<html><head><title>Report</title></head><body><h1>local report body</h1></body></html>`), 0o644); err != nil {
		t.Fatal(err)
	}
	suite, closeFn := NewTools(liveConfig(), ws, tools.NewPathGuard(ws, true, false, nil))
	t.Cleanup(closeFn)
	byName := map[string]tools.Tool{}
	for _, tool := range suite {
		byName[tool.Name()] = tool
	}

	out := mustRun(t, byName["browser_navigate"], map[string]any{"url": "file://" + report})
	if !strings.Contains(out, "local report body") {
		t.Errorf("the local file was not rendered:\n%s", out)
	}
	// A bare absolute path is a file too, never a hostname.
	out = mustRun(t, byName["browser_navigate"], map[string]any{"url": report})
	if !strings.Contains(out, "local report body") {
		t.Errorf("a bare path was not read as a file:\n%s", out)
	}
	// And a path the workspace does not cover is refused as the read it is.
	// The path has to be absolute on the platform running the test: /etc/
	// hostname is not absolute on Windows, so the guard rightly reads it as
	// workspace-relative and the browser reports a missing file instead.
	outside := "/etc/hostname"
	if runtime.GOOS == "windows" {
		outside = filepath.ToSlash(filepath.Join(os.Getenv("SystemRoot"), "win.ini"))
	}
	res := byName["browser_navigate"].Execute(context.Background(), map[string]any{"url": "file:///" + strings.TrimPrefix(outside, "/")})
	if !res.IsError || !strings.Contains(res.ForLLM, "outside workspace") {
		t.Errorf("the guard did not refuse the read: %+v", res)
	}
}

// closeOutOfBand closes a target the way the user closes a tab: through the
// browser, behind the session's back.
func closeOutOfBand(t *testing.T, s *Session, id target.ID) {
	t.Helper()
	s.mu.Lock()
	bctx := s.browserCtx
	s.mu.Unlock()
	c := chromedp.FromContext(bctx)
	if err := target.CloseTarget(id).Do(cdp.WithExecutor(bctx, c.Browser)); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
	// The close is announced asynchronously; the session acts on the notice.
	for range 40 {
		s.mu.Lock()
		held := s.tabs[id]
		s.mu.Unlock()
		if held == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the session never heard that %s was closed", id)
}

// TestATabClosedByHandIsReplaced is the recurring "the browser is hanging"
// report. chromedp drops a closed page from its routing table and cancels
// nothing, so a tab the user closed left the session holding a handle whose
// every command waited for an answer that never came: a full 45-second
// timeout per call, for the life of the process, on a browser that was
// working perfectly.
func TestATabClosedByHandIsReplaced(t *testing.T) {
	requireBrowser(t)
	srv := servePage(t)
	s := NewSession(liveConfig(), t.TempDir(), nil)
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.run(ctx, 20*time.Second, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := s.openTab(ctx); err != nil { // a second tab, as any real browser has
		t.Fatalf("open tab: %v", err)
	}

	closeOutOfBand(t, s, s.currentID())
	if _, err := s.readPage(ctx, "", 5); err != nil {
		t.Fatalf("read after the tab was closed by hand: %v", err)
	}

	// And when the tab closed by hand was the browser's last one, there is no
	// window left to put a tab in, so one has to be opened.
	for _, tab := range mustList(t, s) {
		closeOutOfBand(t, s, tab.ID)
	}
	if _, err := s.readPage(ctx, "", 5); err != nil {
		t.Fatalf("read after every tab was closed by hand: %v", err)
	}
}

func mustList(t *testing.T, s *Session) []tabInfo {
	t.Helper()
	tabs, err := s.listTabs(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return tabs
}

// TestChromedpChatterStaysOutOfTheStandardLog guards the plumbing that keeps a
// dependency's diagnostics from surfacing as unattributed "ERROR:" lines in
// the gateway log. chromedp logs through log.Printf unless told otherwise, and
// a page with a modal on it produces a DOM event this chromedp does not know
// about — which used to read as Factor having failed at something.
func TestChromedpChatterStaysOutOfTheStandardLog(t *testing.T) {
	requireBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><head><title>Modal</title></head><body><dialog id="d">on the top layer</dialog>
<script>document.getElementById('d').showModal()</script></body></html>`)
	}))
	t.Cleanup(srv.Close)

	// slog's default handler writes through the standard logger too, so it
	// has to be sent elsewhere for the buffer to mean "someone bypassed slog".
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var stdlog bytes.Buffer
	log.SetOutput(&stdlog)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	byName := liveSuite(t)
	mustRun(t, byName["browser_navigate"], map[string]any{"url": srv.URL})
	mustRun(t, byName["browser_read"], nil)

	if stdlog.Len() > 0 {
		t.Errorf("chromedp wrote to the standard logger:\n%s", stdlog.String())
	}
}

func TestClickRefusesARefFromAnEarlierPage(t *testing.T) {
	requireBrowser(t)
	// Every read renumbers its refs from e1, so a ref remembered across a
	// navigation resolves to whatever now sits at that position — on a real
	// marketplace, a remembered product link came back as a category menu.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b" {
			fmt.Fprint(w, `<html><head><title>B</title></head><body><main><a href="/wrong">Beta Link</a></main></body></html>`)
			return
		}
		fmt.Fprint(w, `<html><head><title>A</title></head><body><main><a href="/right">Alpha Link</a></main></body></html>`)
	}))
	t.Cleanup(srv.Close)
	byName := liveSuite(t)
	ctx := context.Background()

	res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL + "/a"})
	if res.IsError {
		if strings.Contains(res.ForLLM, "browser start") {
			t.Skipf("chrome cannot start here: %s", res.ForLLM)
		}
		t.Fatalf("navigate: %s", res.ForLLM)
	}
	var ref string
	for _, line := range strings.Split(res.ForLLM, "\n") {
		if strings.Contains(line, `"Alpha Link"`) {
			ref = strings.Fields(strings.TrimSpace(line))[0]
		}
	}
	if ref == "" {
		t.Fatalf("no ref for Alpha Link in:\n%s", res.ForLLM)
	}

	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL + "/b"}); res.IsError {
		t.Fatalf("second navigate: %s", res.ForLLM)
	}
	res = byName["browser_click"].Execute(ctx, map[string]any{"target": ref})
	if !res.IsError {
		t.Fatalf("a ref from the earlier page was clicked without complaint:\n%s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "browser_read") {
		t.Errorf("the refusal does not say how to recover:\n%s", res.ForLLM)
	}
	// A fresh read hands out refs the click accepts again.
	res = byName["browser_read"].Execute(ctx, nil)
	if res.IsError || !strings.Contains(res.ForLLM, `"Beta Link"`) {
		t.Fatalf("re-read after refusal: %s", res.ForLLM)
	}
}
