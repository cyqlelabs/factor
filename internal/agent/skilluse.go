package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/trace"
)

// A learned skill is written once, from one trajectory, and then read by
// every turn its description matches — long after the tools it names have
// changed under it. Three of eleven learned skills on one machine still
// drove playback through paplay a day after the media tool replaced it, and
// nothing ever looked at them again. So every use is judged: a turn the user
// corrected after reading the skill counts against it, a turn that ended
// well clears it, and a skill corrected in skillQuarantineAfter uses in a row
// leaves the catalog. The file stays, with the reason in its frontmatter.

// skillQuarantineAfter is how many uses in a row may be corrected before a
// learned skill is taken out of the catalog. One is chance; two in a row is
// the recipe.
const skillQuarantineAfter = 2

// skillRead is a learned skill a turn read: the version it read, and how
// many corrections the turn had taken by then. Only a correction after the
// read is about the skill.
type skillRead struct {
	hash      string
	steeredAt int
}

// skillUse is one learned skill's record in use: the version last judged and
// how many uses of it in a row were corrected. It lives on disk because two
// uses are rarely one process apart.
type skillUse struct {
	Hash string `json:"hash"`
	Bad  int    `json:"bad"`
}

func skillUsePath() string { return filepath.Join(config.Home(), "skill-uses.json") }

func (l *Loop) skillsRoot() string { return filepath.Join(l.cfg.Agent.Workspace, "skills") }

// noteSkillReads records the learned skills a batch read. The first read in
// a turn is the one that counts.
func (l *Loop) noteSkillReads(tr *trace.Turn, reads map[string]skillRead, calls []provider.ToolCall, outcomes []toolOutcome, steered int) {
	guard := tools.NewPathGuard(l.cfg.Agent.Workspace, false, true, nil)
	// Resolved the way read_file resolves a path, relative to the workspace,
	// so both sides carry the same spelling whatever the config said.
	root, err := guard.Resolve("skills")
	if err != nil {
		return
	}
	for i, call := range calls {
		if call.Name != "read_file" || strings.HasPrefix(outcomes[i].content, "ERROR: ") || outcomes[i].content == interruptedTool {
			continue
		}
		path, err := guard.Resolve(tools.StringArg(call.Args, "path"))
		if err != nil {
			continue
		}
		dir, hash, ok := skills.LearnedFile(root, path)
		if _, seen := reads[dir]; !ok || seen {
			continue
		}
		reads[dir] = skillRead{hash: hash, steeredAt: steered}
		tr.Event(trace.EventSkillRead, dir+" "+hash)
	}
}

// judgeSkillUses settles what a finished turn says about the learned skills
// it read. A correction after the read counts against the skill; a turn that
// ended well clears it; an interrupted, failed or budget-stopped turn says
// nothing about the recipe and changes nothing. A changed file is a new
// skill and starts clean.
func (l *Loop) judgeSkillUses(reads map[string]skillRead, steered int, outcome string) {
	if len(reads) == 0 {
		return
	}
	l.skillUseMu.Lock()
	defer l.skillUseMu.Unlock()
	uses := loadSkillUses()
	for dir, r := range reads {
		u := uses[dir]
		if u.Hash != r.hash {
			u = skillUse{Hash: r.hash}
		}
		switch {
		case steered > r.steeredAt:
			u.Bad++
		case outcome == "ok":
			u.Bad = 0
		default:
			continue
		}
		if u.Bad < skillQuarantineAfter {
			uses[dir] = u
			continue
		}
		reason := fmt.Sprintf("corrected in %d uses in a row, %s", u.Bad, time.Now().Format("2006-01-02"))
		err := skills.Quarantine(l.skillsRoot(), dir, r.hash, reason)
		switch {
		case err == nil:
			slog.Warn("learned skill quarantined: it was corrected in every recent use",
				"skill", dir, "uses", u.Bad, "review", filepath.Join(l.skillsRoot(), dir, "SKILL.md"))
			l.noteWorkspaceChange("quarantined learned skill " + dir)
		case !errors.Is(err, skills.ErrSkillChanged):
			// Kept, so the next corrected use tries again.
			slog.Warn("could not quarantine a learned skill", "skill", dir, "error", err)
			uses[dir] = u
			continue
		}
		delete(uses, dir)
	}
	saveSkillUses(uses)
}

func loadSkillUses() map[string]skillUse {
	uses := map[string]skillUse{}
	data, err := os.ReadFile(skillUsePath())
	if err != nil {
		return uses
	}
	if err := json.Unmarshal(data, &uses); err != nil {
		slog.Warn("ignoring an unreadable skill-use record", "path", skillUsePath(), "error", err)
		return map[string]skillUse{}
	}
	return uses
}

func saveSkillUses(uses map[string]skillUse) {
	data, _ := json.Marshal(uses)
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		slog.Warn("recording learned-skill use", "error", err)
		return
	}
	if err := os.WriteFile(skillUsePath(), data, 0o600); err != nil {
		slog.Warn("recording learned-skill use", "error", err)
	}
}
