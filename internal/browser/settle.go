//go:build !nobrowser

package browser

import (
	"context"
	"time"

	"github.com/chromedp/chromedp"
)

// A click used to be followed by a fixed half-second sleep, a submit by
// seven tenths, a scroll by nine, a back by six — each a guess at how long
// the page needs, wrong in both directions: a same-document toggle is done in
// a frame and paid the whole wait, and a slow navigation is read at its old
// URL because the wait ran out first. What the wait is for is a state
// change, so the wait is now for one: the page is probed before the action
// and again after it, and the read happens as soon as the probe has changed
// and then held still — or at the bound, for an action that legitimately
// changes nothing.
//
// The probe reads four things a same-document change moves as reliably as a
// navigation does: the URL, the document's readyState, how many nodes it
// holds and how long its text is. A menu opening adds nodes; a filter
// applying rewrites text; a navigation moves all four. Comparing the whole
// tuple is what makes the wait notice a change the URL alone would miss.

// pageProbe is the cheap fingerprint one settle wait compares.
type pageProbe struct {
	URL   string `json:"url"`
	Ready string `json:"ready"`
	Nodes int    `json:"nodes"`
	Text  int    `json:"text"`
}

const probeScript = `({
  url: location.href,
  ready: document.readyState,
  nodes: document.getElementsByTagName('*').length,
  text: (document.body ? document.body.innerText : '').length,
})`

// Settle bounds. Vars so the live tests can shrink them; the values are the
// old sleeps as ceilings rather than as durations, and a change that arrives
// in fifty milliseconds is read in fifty milliseconds.
var (
	settleAfterClick  = 1500 * time.Millisecond
	settleAfterSubmit = 2 * time.Second
	settleAfterScroll = 1200 * time.Millisecond
	settleAfterBack   = 1500 * time.Millisecond
	// settlePoll is how often the probe is asked. Two consecutive identical
	// probes after a change is what "held still" means.
	settlePoll = 60 * time.Millisecond
)

// probe reads the page's fingerprint. A failure is returned as a zero probe
// with ok false: the usual cause is a navigation tearing down the execution
// context the evaluate was waiting on, which is itself the change the caller
// is waiting for.
func (s *Session) probe(ctx context.Context) (pageProbe, bool) {
	var p pageProbe
	if err := s.run(ctx, 5*time.Second, chromedp.Evaluate(probeScript, &p)); err != nil {
		return pageProbe{}, false
	}
	return p, true
}

// awaitChange waits until the page differs from before and has then held
// still for one poll, or until bound passes. It reports whether anything
// changed, which is what lets a tool say "the click did nothing visible"
// rather than hand back the same page as if it were news.
func (s *Session) awaitChange(ctx context.Context, before pageProbe, bound time.Duration) (pageProbe, bool) {
	deadline := time.Now().Add(bound)
	last, changed := before, false
	for {
		select {
		case <-ctx.Done():
			return last, changed
		case <-time.After(settlePoll):
		}
		now, ok := s.probe(ctx)
		if !ok {
			// Mid-navigation: the document is going away, which is a change;
			// keep waiting for the one that replaces it to answer.
			changed = true
			if time.Now().After(deadline) {
				return last, changed
			}
			continue
		}
		if now != before {
			changed = true
		}
		if changed && now == last && now.Ready == "complete" {
			return now, true
		}
		if time.Now().After(deadline) {
			return now, changed
		}
		last = now
	}
}

// awaitGrowth waits for the document to grow taller than before, which is
// what a scroll that pulled in lazily loaded content does and a scroll to a
// page that was already all there does not. It returns the height it saw
// last and whether growth arrived inside the bound.
func (s *Session) awaitGrowth(ctx context.Context, before float64, bound time.Duration) (float64, bool) {
	deadline := time.Now().Add(bound)
	last := before
	for {
		select {
		case <-ctx.Done():
			return last, last > before
		case <-time.After(settlePoll):
		}
		var now float64
		if err := s.run(ctx, 5*time.Second, chromedp.Evaluate(`document.body.scrollHeight`, &now)); err == nil {
			if now > before && now == last {
				return now, true // grew, and has stopped growing
			}
			last = now
		}
		if time.Now().After(deadline) {
			return last, last > before
		}
	}
}
