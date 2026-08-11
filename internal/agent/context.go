// Package agent implements the turn loop: one live turn per session,
// mid-turn steering for overflow messages, bounded worker concurrency, and
// a system prompt assembled from identity, workspace bootstrap files,
// drop-in instructions, the skills catalog, and smrti memory recall.
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
)

var bootstrapFiles = []string{"AGENT.md", "SOUL.md", "USER.md"}

// ContextBuilder assembles the system prompt. The static portion (identity,
// bootstrap files, instructions, skill catalog) is cached and invalidated by
// file mtimes; the memory portion is recalled fresh every turn.
type ContextBuilder struct {
	cfg     *config.Config
	skills  *skills.Loader
	ambient *memory.Ambient

	mu     sync.Mutex
	cached string
	stamps map[string]int64
}

func NewContextBuilder(cfg *config.Config, loader *skills.Loader, ambient *memory.Ambient) *ContextBuilder {
	return &ContextBuilder{cfg: cfg, skills: loader, ambient: ambient, stamps: map[string]int64{}}
}

// SystemPrompt builds the full prompt for one turn.
func (cb *ContextBuilder) SystemPrompt(ctx context.Context, history []provider.Message, current string) string {
	parts := []string{cb.staticPart()}
	parts = append(parts, "Current time: "+time.Now().Format("Monday 2006-01-02 15:04 MST"))
	if cb.ambient != nil {
		if mem := cb.ambient.MemoryPrompt(ctx, history, current); mem != "" {
			parts = append(parts, mem)
		}
	}
	return strings.Join(parts, "\n\n")
}

// sourcePaths returns every file whose mtime invalidates the static cache.
func (cb *ContextBuilder) sourcePaths() []string {
	ws := cb.cfg.Agent.Workspace
	paths := make([]string, 0, 8)
	for _, f := range bootstrapFiles {
		paths = append(paths, filepath.Join(ws, f))
	}
	if extra, err := filepath.Glob(filepath.Join(ws, "instructions", "*.md")); err == nil {
		sort.Strings(extra)
		paths = append(paths, extra...)
	}
	if skillFiles, err := filepath.Glob(filepath.Join(ws, "skills", "*", "SKILL.md")); err == nil {
		sort.Strings(skillFiles)
		paths = append(paths, skillFiles...)
	}
	return paths
}

func (cb *ContextBuilder) staticPart() string {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	paths := cb.sourcePaths()
	fresh := map[string]int64{}
	changed := len(paths) != len(cb.stamps)
	for _, p := range paths {
		var stamp int64 = -1
		if info, err := os.Stat(p); err == nil {
			stamp = info.ModTime().UnixNano()
		}
		fresh[p] = stamp
		if cb.stamps[p] != stamp {
			changed = true
		}
	}
	if !changed && cb.cached != "" {
		return cb.cached
	}

	var b strings.Builder
	b.WriteString(cb.identity())
	ws := cb.cfg.Agent.Workspace
	for _, f := range bootstrapFiles {
		if data, err := os.ReadFile(filepath.Join(ws, f)); err == nil && len(data) > 0 {
			b.WriteString("\n\n")
			b.Write(data)
		}
	}
	if extra, err := filepath.Glob(filepath.Join(ws, "instructions", "*.md")); err == nil {
		sort.Strings(extra)
		for _, p := range extra {
			if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
				b.WriteString("\n\n")
				b.Write(data)
			}
		}
	}
	if cb.cfg.Agent.ExtraInstructions != "" {
		b.WriteString("\n\n")
		b.WriteString(cb.cfg.Agent.ExtraInstructions)
	}
	if cb.skills != nil {
		if catalog := cb.skills.Summary(); catalog != "" {
			b.WriteString("\n\n")
			b.WriteString(catalog)
		}
	}

	cb.cached = b.String()
	cb.stamps = fresh
	return cb.cached
}

func (cb *ContextBuilder) identity() string {
	return fmt.Sprintf(`You are Factor, a fast, reliable desktop AI agent and companion running on the user's machine.

Workspace: %s (relative file paths resolve here)

Rules:
- Use tools to act and verify; never claim you did something you didn't do.
- Your long-term memory is real and automatic (smrti). Relevant memories are recalled into this prompt each turn; conversations are stored automatically. Use the remember tool for facts worth keeping, recall to search deliberately, and forget to soften wrong memories. Treat "YOU MUST NOT" memories as hard constraints.
- Never keep the user waiting on slow work: anything likely to take more than ~30 seconds goes through job_start (background), then reply immediately that it's running. You are notified automatically when a job finishes — report the result then. Use the cron tool for recurring schedules.
- Keep replies concise; this is a chat, not a report.`, cb.cfg.Agent.Workspace)
}
