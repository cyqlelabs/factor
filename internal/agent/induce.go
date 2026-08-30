package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/cost"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Skill induction turns a finished multi-tool turn into a reusable skill:
// the trajectory is distilled — off the hot path, on the idle sweep — into a
// SKILL.md in the workspace catalog, so a task worked out once by trial and
// error is a recipe the next time it comes up. The catalog is the retrieval
// mechanism: a learned skill is listed in the system prompt like any other
// and read on demand, so this file only has to write good ones.

const (
	// induceIdleAfter is how long a session must sit quiet before its last
	// qualifying turn is distilled. Long enough that a working session is not
	// billed an induction call per exchange — only the most recent candidate
	// per session survives, so a busy afternoon costs one call when it ends —
	// and shorter than idleCompactAfter because a learned skill is only
	// useful once it is in the catalog.
	induceIdleAfter = 10 * time.Minute
	// induceMinToolCalls is the floor under what counts as a workflow. Below
	// it a turn is a lookup or a one-liner, and a skill distilled from it is
	// a catalog line that costs every future prompt something and teaches
	// nothing.
	induceMinToolCalls = 4
	// induceMaxTokens caps the reply: a recipe that does not fit here is not
	// a recipe.
	induceMaxTokens = 2048
	// The transcript caps: a workflow's shape lives in the calls and their
	// arguments, not in the pages the tools returned, so results are clipped
	// hard and the whole rendering is bounded — an induction call that costs
	// more than the turns it saves has the sign of its own argument wrong.
	induceMaxResultChars = 400
	induceMaxTextChars   = 600
	induceMaxTranscript  = 16000
)

// maxLearnedSkills caps the learned library. Every entry is a line in every
// future prompt and a candidate in every future match, and a library that
// only ever grows converges on noise — the cap is what forces updating over
// minting near-duplicates. A variable so tests can lower it.
var maxLearnedSkills = 40

// induceCandidate is a turn worth considering, rendered at the moment it
// ended: the live messages are in hand there, and holding a bounded string
// beats re-slicing history that compaction may have moved by the time the
// sweep gets to it.
type induceCandidate struct {
	toolCtx    tools.ToolContext
	task       string
	transcript string
}

// noteInduceCandidate remembers the turn that just ended as something the
// idle sweep may distill. Only the most recent qualifying turn per session
// is kept: recurring work recurs, so a candidate displaced today comes back
// the next time the task does, and one call per quiet session is the cost
// ceiling this feature lives under.
func (l *Loop) noteInduceCandidate(in turnInput, messages []provider.Message, thisTurn int, reply string) {
	if !l.cfg.Agent.LearnSkills || in.ephemeral || strings.TrimSpace(reply) == "" {
		return
	}
	if thisTurn <= 0 || thisTurn > len(messages) {
		return
	}
	turn := messages[len(messages)-thisTurn:]
	calls := 0
	for _, m := range turn {
		if m.Role == "assistant" {
			calls += len(m.ToolCalls)
		}
	}
	if calls < induceMinToolCalls {
		return
	}
	task := ""
	for _, m := range turn {
		if m.Role == "user" {
			task = m.Content
			break
		}
	}
	l.induceMu.Lock()
	defer l.induceMu.Unlock()
	l.pendingInduce[in.sessionKey] = induceCandidate{
		toolCtx:    in.toolCtx,
		task:       task,
		transcript: renderTurn(turn),
	}
}

// induceIdleSessions distills the pending candidates whose sessions have
// gone quiet. It runs beside idle compaction on the same sweep, paid while
// nobody is waiting.
func (l *Loop) induceIdleSessions(ctx context.Context) {
	for key, cand := range l.dueInductions() {
		l.wg.Add(1)
		go func(key string, cand induceCandidate) {
			defer l.wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			// The induction is spent on this session's behalf, so the tool
			// context bills it there, same as compaction.
			if err := l.induce(tools.WithToolContext(cctx, cand.toolCtx), key, cand); err != nil {
				slog.Warn("skill induction failed", "session", key, "error", err)
			}
		}(key, cand)
	}
}

// dueInductions claims the candidates ready to run: session not mid-turn and
// idle long enough. Claimed means removed — a failed induction is not worth
// retrying every sweep, because the task that produced it will recur.
func (l *Loop) dueInductions() map[string]induceCandidate {
	l.induceMu.Lock()
	candidates := make(map[string]induceCandidate, len(l.pendingInduce))
	for key, cand := range l.pendingInduce {
		candidates[key] = cand
	}
	l.induceMu.Unlock()

	l.mu.Lock()
	for key := range l.active {
		delete(candidates, key)
	}
	l.mu.Unlock()

	var gone []string
	for key := range candidates {
		last, ok := l.sessions.LastActivity(key)
		if !ok {
			// The session file is gone — cleared under the candidate — so it
			// can never come due; drop it rather than hold its transcript
			// for the life of the process.
			gone = append(gone, key)
			delete(candidates, key)
			continue
		}
		if time.Since(last) < induceIdleAfter {
			delete(candidates, key)
		}
	}

	l.induceMu.Lock()
	for key, cand := range candidates {
		// Claim only what was snapshotted: a turn that ended since may have
		// registered a fresh candidate under this key, and that one has not
		// had its idle wait yet.
		if l.pendingInduce[key] == cand {
			delete(l.pendingInduce, key)
		}
	}
	for _, key := range gone {
		delete(l.pendingInduce, key)
	}
	l.induceMu.Unlock()
	return candidates
}

// induce asks the model whether the candidate turn holds a workflow worth
// keeping and writes the skill it answers with. The decision surface is
// categorical — SKIP, or LEARN with a name — parsed and enforced in code:
// the model proposes, this function disposes.
func (l *Loop) induce(ctx context.Context, sessionKey string, cand induceCandidate) error {
	root := filepath.Join(l.cfg.Agent.Workspace, "skills")
	learned := skills.Learned(root)
	atCap := len(learned) >= maxLearnedSkills

	var catalog []skills.Skill
	if l.builder != nil {
		catalog = l.builder.SkillsCatalog()
	}
	resp, err := l.chat.Chat(ctx, &provider.Request{
		Messages: []provider.Message{
			{Role: "system", Content: inducePrompt},
			{Role: "user", Content: induceInput(cand, catalog, learned, atCap)},
		},
		MaxTokens: induceMaxTokens,
	})
	if err != nil {
		// A cap is a decision the user made; hitting it here is the feature
		// yielding, not breaking.
		if cost.BudgetStop(err) != "" {
			slog.Debug("skill induction skipped by budget", "session", sessionKey)
			return nil
		}
		return fmt.Errorf("induce: %w", err)
	}
	if summaryTruncated(resp.FinishReason) {
		return fmt.Errorf("induction reply hit its token cap; a half-written recipe is worse than none")
	}

	name, desc, body, learn, err := parseInduction(resp.Content)
	if err != nil {
		return err
	}
	if !learn {
		slog.Debug("nothing worth learning from turn", "session", sessionKey)
		return nil
	}
	if atCap {
		isUpdate := false
		for _, s := range learned {
			if s.Name == name {
				isUpdate = true
				break
			}
		}
		if !isUpdate {
			return fmt.Errorf("learned library is at its cap of %d; refusing to add %q", maxLearnedSkills, name)
		}
	}
	path, existed, err := skills.WriteLearned(root, name, desc, body)
	if err != nil {
		return err
	}
	action := "created"
	if existed {
		action = "updated"
	}
	slog.Info("skill learned", "name", name, "action", action, "path", path, "session", sessionKey)
	return nil
}

// renderTurn flattens a turn for the induction prompt. Results are clipped
// hard on purpose: the recipe is which tools were called with what, and a
// clipped result still shows whether the step worked.
func renderTurn(turn []provider.Message) string {
	var b strings.Builder
	for _, m := range turn {
		switch m.Role {
		case "user":
			appendClipped(&b, "user: ", m.Content, induceMaxTextChars)
		case "assistant":
			appendClipped(&b, "assistant: ", m.Content, induceMaxTextChars)
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "tool call: %s%s\n", tc.Name, summarizeArgs(tc.Args))
			}
		case "tool":
			appendClipped(&b, "result: ", m.Content, induceMaxResultChars)
		}
		if b.Len() > induceMaxTranscript {
			b.WriteString("(rest of the turn omitted)\n")
			break
		}
	}
	return b.String()
}

func appendClipped(b *strings.Builder, prefix, text string, limit int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	b.WriteString(prefix)
	b.WriteString(text)
	b.WriteByte('\n')
}

// inducePrompt carries the distillation discipline: sub-routine granularity,
// placeholders for what varies, literal identifiers for what does not, and a
// strong default of learning nothing — most turns teach nothing durable, and
// a wrong skill misleads every future turn that reads it.
const inducePrompt = `A task just finished. Decide whether its trajectory holds a workflow worth keeping as a skill — a procedure likely to be needed again — and if so, write that skill.

Rules:
- Most turns teach nothing durable. A one-off errand, a plain question, or a task an existing skill already covers gets SKIP. When in doubt, SKIP.
- Extract the reusable sub-routine, not this task. Replace task-specific values with {placeholder} names; keep invariant identifiers — tool names, URLs, paths, commands that worked — exactly as they appear.
- Write the body as numbered steps naming the tools to call, at least two. If the trajectory shows attempts that failed before one worked, add a Pitfalls section saying what not to repeat.
- The skill is read in every future conversation: no personal data, no secrets — procedure only.
- To improve a skill from the learned list, reply with its exact name; the new body replaces the old, so carry forward what it already gets right. Never use any other existing skill's name.

Reply in exactly one of these two forms and nothing else:

SKIP

or:

LEARN
name: short-kebab-case-name
description: one line saying when to use it
---
the markdown body`

// induceInput assembles the case: the task, the trajectory, and the catalog
// state the reply has to respect.
func induceInput(cand induceCandidate, catalog, learned []skills.Skill, atCap bool) string {
	isLearned := make(map[string]bool, len(learned))
	var b strings.Builder
	b.WriteString("# Task\n")
	b.WriteString(strings.TrimSpace(cand.task))
	b.WriteString("\n\n# Trajectory\n")
	b.WriteString(cand.transcript)
	b.WriteString("\n# Learned skills (the only names you may reuse, to update)\n")
	if len(learned) == 0 {
		b.WriteString("(none yet)\n")
	}
	for _, s := range learned {
		isLearned[s.Name] = true
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	others := 0
	for _, s := range catalog {
		if isLearned[s.Name] {
			continue
		}
		if others == 0 {
			b.WriteString("\n# Other skills (do not reuse their names; SKIP if one already covers this)\n")
		}
		others++
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	if atCap {
		b.WriteString("\nThe learned library is full: update one of the learned skills or reply SKIP.\n")
	}
	return b.String()
}

// parseInduction reads the two-form reply. The format is parsed, not
// trusted: anything fitting neither form is an error and the caller drops
// it, because a skill file assembled from a guess pollutes the catalog for
// the life of the workspace.
func parseInduction(reply string) (name, desc, body string, learn bool, err error) {
	text := strings.TrimSpace(reply)
	// Models fence output unasked; strip one enclosing fence before parsing.
	if strings.HasPrefix(text, "```") {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	}
	head, rest, _ := strings.Cut(text, "\n")
	switch {
	case strings.HasPrefix(strings.TrimSpace(head), "SKIP"):
		return "", "", "", false, nil
	case !strings.HasPrefix(strings.TrimSpace(head), "LEARN"):
		return "", "", "", false, fmt.Errorf("induction reply opens with neither SKIP nor LEARN")
	}
	lines := strings.Split(rest, "\n")
	cut := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			cut = i
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.TrimSpace(value)
		case "description":
			desc = strings.TrimSpace(value)
		}
	}
	if cut < 0 {
		return "", "", "", false, fmt.Errorf("induction reply has no --- separator before the body")
	}
	body = strings.TrimSpace(strings.Join(lines[cut+1:], "\n"))
	if name == "" || desc == "" || body == "" {
		return "", "", "", false, fmt.Errorf("induction reply is missing a name, description, or body")
	}
	return name, desc, body, true, nil
}
