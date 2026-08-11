// Package heartbeat periodically checks HEARTBEAT.md for user-defined tasks.
// No actionable content → no LLM call. A reply of exactly HEARTBEAT_OK is
// suppressed; anything else is delivered to the last active channel.
package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const OKToken = "HEARTBEAT_OK"

// Runner executes a history-less agent turn.
type Runner func(ctx context.Context, prompt string) (string, error)

// Deliver sends a non-OK heartbeat result to the user.
type Deliver func(content string) bool

type Service struct {
	workspace string
	interval  time.Duration
	run       Runner
	deliver   Deliver
}

func NewService(workspace string, interval time.Duration, run Runner, deliver Deliver) *Service {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	return &Service{workspace: workspace, interval: interval, run: run, deliver: deliver}
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
	content, err := os.ReadFile(filepath.Join(s.workspace, "HEARTBEAT.md"))
	if err != nil {
		return
	}
	if !HasActionable(string(content)) {
		return // no tasks → no LLM call
	}
	prompt := fmt.Sprintf(`# Heartbeat check

This is an automated periodic check, not a user message. Work through the
tasks in your HEARTBEAT.md below using your tools. If nothing needs attention
or user notification right now, reply exactly %s.

%s`, OKToken, content)

	reply, err := s.run(ctx, prompt)
	if err != nil {
		slog.Warn("heartbeat turn failed", "error", err)
		return
	}
	reply = strings.TrimSpace(reply)
	if reply == "" || reply == OKToken || strings.HasPrefix(reply, OKToken) {
		return
	}
	if s.deliver != nil && !s.deliver(reply) {
		slog.Info("heartbeat result had no delivery target", "reply_len", len(reply))
	}
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
