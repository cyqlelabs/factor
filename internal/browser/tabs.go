//go:build !nobrowser

package browser

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/cyqlelabs/factor/internal/tools"
)

// tabHandle is one tab this session can drive. owned marks a tab Factor
// opened itself, and it is the whole reason this type exists: chromedp closes
// a tab when its context is cancelled, so cancelling the context of a tab the
// user already had open would close their tab out from under them. Only owned
// tabs are ever cancelled implicitly; an adopted one is released by dropping
// the reference and letting the connection die with the process.
type tabHandle struct {
	ctx    context.Context
	cancel context.CancelFunc
	id     target.ID
	owned  bool
}

// maxTabsInRead bounds the tab list appended to a page read. A browser with
// forty tabs open should cost the model a line, not a page.
const maxTabsInRead = 8

type tabInfo struct {
	Index   int
	ID      target.ID
	Title   string
	URL     string
	Current bool
}

// targets asks the browser — not the tab — what it has open.
func (s *Session) targets(ctx context.Context, timeout time.Duration) ([]*target.Info, error) {
	tab, err := s.ensure()
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(tab, timeout)
	defer cancel()
	type result struct {
		infos []*target.Info
		err   error
	}
	done := make(chan result, 1)
	go func() {
		infos, err := chromedp.Targets(runCtx)
		done <- result{infos, err}
	}()
	select {
	case r := <-done:
		return r.infos, r.err
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

// interesting reports whether a target is a tab a person would recognize.
// DevTools windows, extension pages and service workers are all "targets" the
// protocol will happily list, and none of them is somewhere to browse.
func interesting(t *target.Info) bool {
	if t.Type != "page" {
		return false
	}
	for _, prefix := range []string{"devtools://", "chrome-extension://", "chrome://"} {
		if strings.HasPrefix(t.URL, prefix) {
			return false
		}
	}
	return true
}

// listTabs numbers the open tabs and records the numbering, so a later
// switch by index means what the model just read.
func (s *Session) listTabs(ctx context.Context) ([]tabInfo, error) {
	infos, err := s.targets(ctx, 15*time.Second)
	if err != nil {
		return nil, err
	}
	current := s.currentID()
	var tabs []tabInfo
	for _, t := range infos {
		if !interesting(t) {
			continue
		}
		tabs = append(tabs, tabInfo{
			Index:   len(tabs) + 1,
			ID:      t.TargetID,
			Title:   strings.TrimSpace(t.Title),
			URL:     t.URL,
			Current: t.TargetID == current,
		})
	}
	s.mu.Lock()
	s.tabRefs = map[int]target.ID{}
	for _, t := range tabs {
		s.tabRefs[t.Index] = t.ID
	}
	s.mu.Unlock()
	return tabs, nil
}

func (s *Session) currentID() target.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return ""
	}
	return s.cur.id
}

// resolveTab turns what the model said into a tab: the index from the last
// listing, or a substring of a title or URL. Matching on text is what makes
// this usable at all — indexes shift when the user opens a tab mid-turn,
// "gmail" does not.
func (s *Session) resolveTab(tabs []tabInfo, query string) (tabInfo, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return tabInfo{}, fmt.Errorf("which tab? pass target as the number from the list, or text matching its title or URL")
	}
	if n, err := strconv.Atoi(query); err == nil {
		for _, t := range tabs {
			if t.Index == n {
				return t, nil
			}
		}
		return tabInfo{}, fmt.Errorf("no tab %d is open; %d tabs are", n, len(tabs))
	}
	needle := strings.ToLower(query)
	var hits []tabInfo
	for _, t := range tabs {
		if strings.Contains(strings.ToLower(t.Title), needle) || strings.Contains(strings.ToLower(t.URL), needle) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return tabInfo{}, fmt.Errorf("no open tab matches %q", query)
	case 1:
		return hits[0], nil
	}
	var names []string
	for _, t := range hits {
		names = append(names, fmt.Sprintf("%d %q", t.Index, t.Title))
	}
	return tabInfo{}, fmt.Errorf("%q matches %d tabs (%s) — say which by number", query, len(hits), strings.Join(names, ", "))
}

// switchTo makes an already-open tab the one every browser tool acts on.
func (s *Session) switchTo(ctx context.Context, id target.ID) error {
	if _, err := s.ensure(); err != nil { // the browser connection, not the tab
		return err
	}
	s.mu.Lock()
	h, ok := s.tabs[id]
	if ok && h.ctx.Err() != nil {
		delete(s.tabs, id)
		ok = false
	}
	if !ok {
		var err error
		if h, err = s.attachLocked(id, false); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.cur = h
	s.refs = map[string]string{} // refs belong to the page they were read from
	s.mu.Unlock()

	// Attach now so a dead target fails here rather than inside the next tool,
	// and raise the tab for the user watching the screen.
	if err := s.run(ctx, 20*time.Second, chromedp.ActionFunc(func(c context.Context) error {
		return page.BringToFront().Do(c)
	})); err != nil {
		s.mu.Lock()
		delete(s.tabs, id)
		s.cur = nil
		s.mu.Unlock()
		return err
	}
	return nil
}

// closeTab closes a tab and leaves the session pointing somewhere live.
func (s *Session) closeTab(ctx context.Context, id target.ID) error {
	s.mu.Lock()
	h, held := s.tabs[id]
	if held {
		delete(s.tabs, id)
		if s.cur == h {
			s.cur = nil
		}
	}
	browserCtx := s.browserCtx
	s.mu.Unlock()

	if browserCtx == nil {
		return fmt.Errorf("no browser connection")
	}
	// A tab is closed by releasing the context attached to it: chromedp
	// intercepts the protocol's own close command and refuses it, so a tab
	// nobody is attached to has to be attached to first. This is the one
	// place cancelling an adopted tab is the right thing — closing it is
	// exactly what was asked for.
	if !held {
		s.mu.Lock()
		var err error
		h, err = s.attachLocked(id, false)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		delete(s.tabs, id)
		s.mu.Unlock()
	}
	// Cancel takes its deadline from the context it is handed, so the bound
	// rides the tab's own context rather than wrapping the call.
	cancelCtx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Cancel(cancelCtx); err != nil {
		return err
	}
	return s.awaitTabGone(ctx, id)
}

// awaitTabGone waits for the browser to actually drop the target. Closing is
// asynchronous: the command is acknowledged before the tab disappears, and
// reporting a close that has not happened yet makes the very next listing
// contradict what was just said.
func (s *Session) awaitTabGone(ctx context.Context, id target.ID) error {
	for attempt := range 20 {
		if attempt > 0 {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		infos, err := s.targets(ctx, 10*time.Second)
		if err != nil {
			return nil // the tab was asked to close; a failed re-check is not proof it did not
		}
		gone := true
		for _, t := range infos {
			if t.TargetID == id {
				gone = false
				break
			}
		}
		if gone {
			return nil
		}
	}
	return fmt.Errorf("the browser did not close the tab")
}

// otherTabs renders the tabs the model is not looking at, for the footer of
// every page read. This is the line that makes a tab the user already has
// open — logged into the account they meant — something the model knows
// exists at all, instead of a thing it can only find by looking at the screen.
func (s *Session) otherTabs(ctx context.Context) []string {
	tabs, err := s.listTabs(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range tabs {
		if t.Current {
			continue
		}
		if len(out) == maxTabsInRead {
			out = append(out, fmt.Sprintf("  … and %d more", len(tabs)-1-maxTabsInRead))
			break
		}
		out = append(out, fmt.Sprintf("  %d %q %s", t.Index, t.Title, t.URL))
	}
	return out
}

type tabsTool struct{ s *Session }

func (t *tabsTool) Name() string { return "browser_tabs" }
func (t *tabsTool) Description() string {
	return "List the browser's open tabs and switch between them. This is the user's own browser, so the tab they are talking about is usually already open and already signed in to the right account — switch to it rather than opening a duplicate, and check which account a page shows before acting on it. action='list' shows every tab, 'switch' moves there (by number or by text matching the title or URL), 'open' starts a new tab, 'close' closes one."
}
func (t *tabsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []any{"list", "switch", "open", "close"}, "description": "What to do (default list)"},
			"target": map[string]any{"type": "string", "description": "For switch and close: the tab number from the list, or text matching its title or URL"},
			"url":    map[string]any{"type": "string", "description": "For open: the URL to load in the new tab"},
		},
	}
}

func (t *tabsTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	switch action := tools.StringArg(args, "action"); action {
	case "", "list":
		return t.list(ctx)
	case "switch":
		return t.switchTab(ctx, tools.StringArg(args, "target"))
	case "open":
		return t.open(ctx, tools.StringArg(args, "url"))
	case "close":
		return t.close(ctx, tools.StringArg(args, "target"))
	default:
		return tools.Errorf("unknown action %q — use list, switch, open or close", action)
	}
}

func formatTabs(tabs []tabInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d open tabs:\n", len(tabs))
	for _, t := range tabs {
		marker := " "
		if t.Current {
			marker = "*"
		}
		fmt.Fprintf(&b, "%s %d %q %s\n", marker, t.Index, t.Title, t.URL)
	}
	b.WriteString("(* is the tab the browser tools are acting on)")
	return b.String()
}

func (t *tabsTool) list(ctx context.Context) *tools.Result {
	tabs, err := t.s.listTabs(ctx)
	if err != nil {
		return tools.Errorf("listing tabs failed: %v", err)
	}
	if len(tabs) == 0 {
		return tools.Text("No tabs are open.")
	}
	return tools.Text(formatTabs(tabs))
}

func (t *tabsTool) switchTab(ctx context.Context, query string) *tools.Result {
	tabs, err := t.s.listTabs(ctx)
	if err != nil {
		return tools.Errorf("listing tabs failed: %v", err)
	}
	tab, err := t.s.resolveTab(tabs, query)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if err := t.s.switchTo(ctx, tab.ID); err != nil {
		return tools.Errorf("switching to tab %d failed: %v", tab.Index, err)
	}
	r, err := t.s.read(ctx)
	if err != nil {
		return tools.Errorf("switched to tab %d, but read failed: %v", tab.Index, err)
	}
	return tools.Text(formatRead(r))
}

func (t *tabsTool) open(ctx context.Context, url string) *tools.Result {
	if err := t.s.openTab(ctx); err != nil {
		return tools.Errorf("opening a tab failed: %v", err)
	}
	if strings.TrimSpace(url) == "" {
		return tools.Text("Opened a new tab.")
	}
	return (&navigateTool{t.s}).Execute(ctx, map[string]any{"url": url})
}

func (t *tabsTool) close(ctx context.Context, query string) *tools.Result {
	tabs, err := t.s.listTabs(ctx)
	if err != nil {
		return tools.Errorf("listing tabs failed: %v", err)
	}
	if len(tabs) <= 1 {
		return tools.Errorf("refusing to close the only open tab")
	}
	tab, err := t.s.resolveTab(tabs, query)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if err := t.s.closeTab(ctx, tab.ID); err != nil {
		return tools.Errorf("closing tab %d failed: %v", tab.Index, err)
	}
	return tools.Textf("Closed tab %d %q.", tab.Index, tab.Title)
}
