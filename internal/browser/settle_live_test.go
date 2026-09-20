//go:build !nobrowser

package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

// settlePage has one control per kind of wait: a click that changes the
// document in place, one that changes nothing, one that navigates, a field
// whose Enter renders results after a delay, and a list that grows on scroll.
const settlePage = `<html><head><title>Settle</title></head><body>
<main>
  <input id="q" type="text" placeholder="Query" value="prefilled">
  <button id="toggle" onclick="document.getElementById('out').innerText='toggled'">Toggle</button>
  <button id="noop" onclick="void 0">Noop</button>
  <a id="away" href="/other">Other page</a>
  <div id="out"></div>
  <div style="height:3000px">tall</div>
  <div id="list"></div>
</main>
<script>
  document.getElementById('q').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { setTimeout(() => { document.getElementById('out').innerText = 'results for ' + e.target.value; }, 300); }
  });
  window.addEventListener('scroll', () => {
    if (!window.grew) { window.grew = true; setTimeout(() => { const d = document.createElement('div'); d.style.height = '2000px'; d.innerText = 'more'; document.getElementById('list').appendChild(d); }, 200); }
  });
</script>
</body></html>`

func settleSuite(t *testing.T) (*Session, string) {
	t.Helper()
	requireBrowser(t)
	srv := serveHTML(t, settlePage)
	s := NewSession(liveConfig(), t.TempDir(), nil)
	t.Cleanup(s.Close)
	res := (&navigateTool{s}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if res.IsError {
		if strings.Contains(res.ForLLM, "browser start") {
			t.Skipf("chrome cannot start here: %s", res.ForLLM)
		}
		t.Fatal(res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"Query" = "prefilled"`) {
		t.Errorf("the read does not carry the field's value:\n%s", res.ForLLM)
	}
	return s, srv.URL
}

// A click that changes the document in place is read as soon as it has,
// not after a fixed half second; one that changes nothing says so.
func TestClickWaitsForTheChangeAndReportsNone(t *testing.T) {
	s, _ := settleSuite(t)
	ctx := context.Background()
	started := time.Now()
	r, changed, err := s.click(ctx, "#toggle")
	if err != nil || !changed || !strings.Contains(r.Text, "toggled") {
		t.Fatalf("toggle: changed=%v err=%v text=%q", changed, err, r.Text)
	}
	if took := time.Since(started); took > settleAfterClick {
		t.Errorf("a same-document change was waited out to the bound: %v", took)
	}
	res := (&clickTool{s}).Execute(ctx, map[string]any{"target": "#noop"})
	if res.IsError || !strings.Contains(res.ForLLM, "changed nothing visible") {
		t.Errorf("noop click: %+v", res)
	}
	// The same toggle again leaves the page as it is, and is reported so:
	// what the note tracks is whether the page moved, not whether the
	// control is one that ever did.
	res = (&clickTool{s}).Execute(ctx, map[string]any{"target": "#toggle"})
	if res.IsError || !strings.Contains(res.ForLLM, "changed nothing visible") {
		t.Errorf("a repeated toggle that moved nothing was not reported: %+v", res)
	}
}

// A click that navigates is read on the new page rather than the old one.
func TestClickThatNavigatesIsReadOnTheNewPage(t *testing.T) {
	s, _ := settleSuite(t)
	r, changed, err := s.click(context.Background(), "#away")
	if err != nil || !changed {
		t.Fatalf("navigate click: changed=%v err=%v", changed, err)
	}
	if !strings.HasSuffix(r.URL, "/other") {
		t.Errorf("read at %q, want the page the click went to", r.URL)
	}
}

// A submit waits for the result it triggers, even one the page renders
// asynchronously, so the read that follows sees it.
func TestFillWithSubmitWaitsForTheResult(t *testing.T) {
	s, _ := settleSuite(t)
	ctx := context.Background()
	msg, err := s.fill(ctx, "#q", "flights", true)
	if err != nil || !strings.Contains(msg, `Filled #q with "flights"`) {
		t.Fatalf("fill: %q %v", msg, err)
	}
	r, err := s.read(ctx)
	if err != nil || !strings.Contains(r.Text, "results for flights") {
		t.Errorf("the read after submit does not show the async result: %q", r.Text)
	}
}

// A scroll that pulls in more of the page waits for the growth and says so.
func TestScrollWaitsForLazyContent(t *testing.T) {
	s, _ := settleSuite(t)
	grew, r, err := s.scroll(context.Background(), "bottom", "")
	if err != nil {
		t.Fatal(err)
	}
	if grew == "" || !strings.Contains(grew, "loaded more of the page") {
		t.Errorf("growth not reported: %q", grew)
	}
	if !strings.Contains(r.Text, "more") {
		t.Errorf("the read after the scroll lacks the lazily added content: %q", r.Text)
	}
	grew, _, err = s.scroll(context.Background(), "top", "")
	if err != nil || grew != "" {
		t.Errorf("a scroll on a page that is all there reported growth: %q %v", grew, err)
	}
}

// The probe itself: identical pages compare equal, a change moves it, and a
// dead context answers not-ok rather than a zero probe read as a change.
func TestProbeAndAwaitChange(t *testing.T) {
	s, _ := settleSuite(t)
	ctx := context.Background()
	before, ok := s.probe(ctx)
	if !ok || before.URL == "" || before.Nodes == 0 || before.Ready != "complete" {
		t.Fatalf("probe = %+v ok=%v", before, ok)
	}
	if _, changed := s.awaitChange(ctx, before, 150*time.Millisecond); changed {
		t.Error("an untouched page read as changed")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _, _ = s.click(context.Background(), "#toggle")
	}()
	after, changed := s.awaitChange(ctx, before, 2*time.Second)
	if !changed || after == before {
		t.Errorf("a change under the wait was missed: %+v", after)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := s.probe(cancelled); ok {
		t.Error("a probe on a cancelled context answered ok")
	}
}
