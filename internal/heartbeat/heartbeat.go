// Package heartbeat periodically checks HEARTBEAT.md for user-defined tasks,
// and Factor's own numbers for ones that have drifted. Nothing to look at →
// no LLM call. A reply carrying HEARTBEAT_OK is suppressed; anything else is
// delivered to the last active channel. Carrying, not equal to: the model is
// asked for exactly the token and answers with a paragraph of diagnosis and
// the token after it, which is still its verdict that nothing needs the user,
// and was being read out to them as a finding.
//
// The two triggers are deliberately different in kind. HEARTBEAT.md is what
// the user thought to write down in advance; the control bands are what
// actually changed. Detection for the second stays entirely deterministic —
// the model is spent on diagnosing a breach, never on confirming that nothing
// happened.
package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/bands"
)

const OKToken = "HEARTBEAT_OK"

// Runner executes a history-less agent turn.
type Runner func(ctx context.Context, prompt string) (string, error)

// Deliver sends a non-OK heartbeat result to the user.
type Deliver func(content string) bool

// Bands reports the metrics that have drifted out of their band. Nil is the
// old behaviour: HEARTBEAT.md is the only thing that can start a check.
type Bands func() []bands.Breach

type Service struct {
	workspace string
	interval  time.Duration
	run       Runner
	deliver   Deliver
	bands     Bands
}

func NewService(workspace string, interval time.Duration, run Runner, deliver Deliver) *Service {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	return &Service{workspace: workspace, interval: interval, run: run, deliver: deliver}
}

// WithBands gives the heartbeat something to notice besides what the user
// wrote down.
func (s *Service) WithBands(b Bands) *Service {
	s.bands = b
	return s
}

// Run ticks until ctx is done.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick performs one heartbeat check (exported for tests and manual trigger).
func (s *Service) Tick(ctx context.Context) {
	// A missing HEARTBEAT.md is not a reason to skip the bands: a user who
	// never wrote one still wants to hear that their spend tripled.
	raw, _ := os.ReadFile(filepath.Join(s.workspace, "HEARTBEAT.md"))
	content := string(raw)

	var breached []bands.Breach
	if s.bands != nil {
		breached = s.bands()
	}
	if !HasActionable(content) && len(breached) == 0 {
		return // nothing written down, nothing drifting → no LLM call
	}
	for _, b := range breached {
		slog.Info("control band breached", "metric", b.Metric, "sigma", b.Sigma,
			"recent", b.Recent, "baseline", b.Baseline, "tier", b.Tier())
	}

	prompt := fmt.Sprintf(`# Heartbeat check

This is an automated periodic check, not a user message. Work through the
tasks in your HEARTBEAT.md below using your tools. If nothing needs attention
or user notification right now, reply exactly %s.

%s%s`, OKToken, content, bandSection(breached))

	reply, err := s.run(ctx, prompt)
	if err != nil {
		slog.Warn("heartbeat turn failed", "error", err)
		return
	}
	reply = strings.TrimSpace(reply)
	if reply == "" || strings.Contains(reply, OKToken) {
		return
	}
	if s.deliver != nil && !s.deliver(reply) {
		slog.Info("heartbeat result had no delivery target", "reply_len", len(reply))
	}
}

// bandSection describes the drifted metrics to the turn that is about to look
// at them. The numbers are stated and the conclusion is not: the detection is
// arithmetic over Factor's own traces and says only that something moved, and
// whether that matters is exactly the judgement the model is being spent on.
//
// Tier 3 is called out because it is the one worth interrupting the user for;
// below that the reading is unusual but not yet a story.
func bandSection(breached []bands.Breach) string {
	if len(breached) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Drifted metrics (measured, not diagnosed)\n")
	b.WriteString("These are Factor's own numbers over the last hour against a rolling baseline. ")
	b.WriteString("Work out whether any of them matters. Say something only if it does; ")
	b.WriteString("an unusual number with a dull explanation is not worth the user's attention.\n")
	for _, x := range breached {
		fmt.Fprintf(&b, "- %s%s\n", x.Line(), tierNote(x))
	}
	return b.String()
}

func tierNote(b bands.Breach) string {
	if b.Tier() >= 3 {
		return " — well outside the usual range"
	}
	return ""
}

// HasActionable reports whether the file contains task-like lines: list
// items or checkboxes outside comments.
func HasActionable(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [x]") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- [ ]") {
			return true
		}
	}
	return false
}
