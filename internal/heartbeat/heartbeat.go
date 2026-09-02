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
//
// Diagnosing needs three things a bare number does not give, and the first
// breach this ever fired on showed all three missing. Handed "tool error rate
// is 53%", the model grepped the journal, which holds no tool outcomes, and
// called transient a tool that had failed every time it ran; it could have
// switched the feature off with a tool it already had; and the next breach
// would have got the same shrug, because nothing remembered this one. So a
// breach arrives with its evidence, the check is told to repair before it
// reports, and what it concluded is kept and shown to the next check on the
// same metric.
package heartbeat

import (
	"bufio"
	"context"
	"encoding/json"
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
	verdicts  string // where each breach's verdict is kept; blank keeps none
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

// WithVerdicts keeps what each check concluded about a breach in the file at
// path, so the next check on the same metric reads what was said last time
// and a fault that keeps coming back is seen to keep coming back.
func (s *Service) WithVerdicts(path string) *Service {
	s.verdicts = path
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

%s%s%s`, OKToken, content, bandSection(breached), s.earlierVerdicts(breached))

	reply, err := s.run(ctx, prompt)
	if err != nil {
		slog.Warn("heartbeat turn failed", "error", err)
		return
	}
	reply = strings.TrimSpace(reply)
	if len(breached) > 0 {
		s.recordVerdict(breached, reply)
	}
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
// What it is judged from rides along — the calls that failed, what they said,
// and each failing tool's record over the baseline — because a number alone
// sent the model to the journal, which holds no tool outcomes.
//
// Tier 3 is called out because it is the one worth interrupting the user for;
// below that the reading is unusual but not yet a story.
func bandSection(breached []bands.Breach) string {
	if len(breached) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Drifted metrics (measured, not diagnosed)\n")
	b.WriteString("These are Factor's own numbers over the last hour against a rolling baseline, ")
	b.WriteString("with the evidence behind each: the calls that failed and what they said, and each failing tool's record over the whole baseline. ")
	b.WriteString("That is the entire record — the traces hold nothing more and the journal holds no tool outcomes — so judge from it rather than going looking. ")
	b.WriteString("Work out whether any of it matters. Say something only if it does; ")
	b.WriteString("an unusual number with a dull explanation is not worth the user's attention.\n")
	for _, x := range breached {
		fmt.Fprintf(&b, "- %s%s\n", x.Line(), tierNote(x))
		for _, d := range x.Details() {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
	}
	b.WriteString(repairGuidance)
	return b.String()
}

// repairGuidance is what the check is expected to do with a breach that
// matters: fix it with the levers it owns, then report. The levers are named
// because the ones it must not reach for are the obvious ones — an exec that
// kills a supervised sidecar leaves an orphan behind, which has happened.
const repairGuidance = `
Then act before you report. A tool that has failed every time it ran on this machine is a broken feature, not a transient: switch it off with config_set (read the key with config_get first) or repair what it needs, rather than judging it again next time. A version that is behind is upgraded with the upgrade tool; a missing helper is installed with pkg_install. Never kill or restart processes with exec — the sidecars are supervised, and one you start by hand outlives your turn as an orphan. What you could not fix, or what only the user can decide, goes to them in a line or two with the cause named. Do not write HEARTBEAT_OK in a reply that reports a change or a finding, since it silences the whole reply; HEARTBEAT_OK alone is for when nothing was worth changing and nothing needs the user.
`

// verdict is what one check concluded about the metrics that had drifted.
type verdict struct {
	At      time.Time `json:"at"`
	Metrics []string  `json:"metrics"`
	Reply   string    `json:"reply"`
}

// Bounds on the verdict log: what is kept on disk, how far back a check is
// shown, and how many. Five verdicts on one metric is a pattern; fifty on
// disk is a fortnight of a badly behaved box.
const (
	keepVerdicts    = 50
	showVerdicts    = 5
	verdictAge      = 14 * 24 * time.Hour
	maxVerdictChars = 600
)

// recordVerdict appends what the check said about this breach.
func (s *Service) recordVerdict(breached []bands.Breach, reply string) {
	if s.verdicts == "" {
		return
	}
	// One line per verdict: it is read back as a bullet in a prompt.
	reply = strings.Join(strings.Fields(reply), " ")
	if r := []rune(reply); len(r) > maxVerdictChars {
		reply = string(r[:maxVerdictChars]) + "…"
	}
	all := append(s.loadVerdicts(), verdict{At: time.Now(), Metrics: metricsOf(breached), Reply: reply})
	if len(all) > keepVerdicts {
		all = all[len(all)-keepVerdicts:]
	}
	var b strings.Builder
	for _, v := range all {
		raw, err := json.Marshal(v)
		if err != nil {
			continue
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(s.verdicts, []byte(b.String()), 0o600); err != nil {
		slog.Warn("could not keep the heartbeat's verdict", "path", s.verdicts, "error", err)
	}
}

// earlierVerdicts is what was concluded the last few times any of these
// metrics drifted, for the check about to judge them again.
func (s *Service) earlierVerdicts(breached []bands.Breach) string {
	if s.verdicts == "" || len(breached) == 0 {
		return ""
	}
	past := s.verdictsOn(metricsOf(breached))
	if len(past) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# What you concluded the last times these metrics drifted\n")
	for _, v := range past {
		fmt.Fprintf(&b, "- %s (%s): %s\n", v.At.Format("2006-01-02 15:04"), strings.Join(v.Metrics, ", "), v.Reply)
	}
	b.WriteString("A reading that keeps coming back is not transient, whatever was said before: name the cause and deal with it.\n")
	return b.String()
}

// verdictsOn is the newest few recent verdicts that touched any of metrics.
func (s *Service) verdictsOn(metrics []string) []verdict {
	current := map[string]bool{}
	for _, m := range metrics {
		current[m] = true
	}
	now := time.Now()
	var past []verdict
	for _, v := range s.loadVerdicts() {
		if now.Sub(v.At) > verdictAge {
			continue
		}
		for _, m := range v.Metrics {
			if current[m] {
				past = append(past, v)
				break
			}
		}
	}
	if len(past) > showVerdicts {
		past = past[len(past)-showVerdicts:]
	}
	return past
}

// loadVerdicts reads the log, forgiving a torn line the way the trace reader
// does: a verdict that cannot be read is one fewer to show, not a reason to
// show none.
func (s *Service) loadVerdicts() []verdict {
	f, err := os.Open(s.verdicts)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []verdict
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var v verdict
		if json.Unmarshal(sc.Bytes(), &v) == nil && !v.At.IsZero() {
			out = append(out, v)
		}
	}
	return out
}

func metricsOf(breached []bands.Breach) []string {
	out := make([]string, 0, len(breached))
	for _, b := range breached {
		out = append(out, b.Metric)
	}
	return out
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
