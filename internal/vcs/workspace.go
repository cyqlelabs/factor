// Package vcs keeps a local git history of the agent's workspace.
//
// Factor's own prompt is compiled into the binary and versioned with it. The
// user's half is not: AGENTS.md, SOUL.md, USER.md, HEARTBEAT.md, the
// instructions and the skill library all live in ~/.factor/workspace with no
// history at all. Somebody who tunes their agent's persona over six months
// has no diff, no blame and no way back — and skill induction writes into
// that same library on its own, so the one file nobody chose to change is
// also the one hardest to undo.
//
// This is a plain local repository with no remote and no network. It exists
// so a bad edit is revertible and a change is attributable, which is what the
// rest of Factor's records already give for everything except the part the
// agent edits about itself.
//
// Two things are deliberately left out. Session transcripts churn on every
// message and would bury the changes worth reading, so sessions/ is ignored.
// And config.json is not here at all: it holds API keys by design, and
// copying them into git objects to gain a diff is a bad trade.
package vcs

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// gitTimeout bounds one git invocation. A commit of a few markdown files is
// milliseconds; anything near this is a repository problem, and the caller is
// a tool call that must not hang on it.
const gitTimeout = 15 * time.Second

// ignore is what never belongs in the history.
const ignore = `# Session transcripts churn on every message and would bury the
# changes worth reading. Everything else here is the agent's own context.
sessions/
*.archive.jsonl
`

// Repo is the workspace's history. A nil Repo is versioning switched off, and
// every method is safe on one.
type Repo struct {
	dir string
	mu  sync.Mutex
}

// Open prepares the workspace repository, or returns nil when versioning is
// off, git is unavailable, or the repository cannot be created. None of those
// is an error worth failing startup over: the workspace works exactly as it
// did before, and the user is told once why there is no history.
func Open(dir string, enabled bool) *Repo {
	if !enabled || dir == "" {
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		slog.Info("workspace versioning is on but git is not installed; continuing without a history")
		return nil
	}
	r := &Repo{dir: dir}
	if err := r.init(); err != nil {
		slog.Warn("could not prepare the workspace history; continuing without one", "error", err)
		return nil
	}
	return r
}

// init creates the repository if the workspace does not already have one of
// its own.
//
// Adoption is deliberately narrow: only a .git directly inside the workspace
// counts. Asking git whether it is in a repository would answer yes from
// inside somebody's dotfiles checkout, and Factor would start committing the
// agent's scratch work into it.
func (r *Repo) init() error {
	if _, err := os.Stat(filepath.Join(r.dir, ".git")); err == nil {
		return r.writeIgnore()
	}
	if _, err := r.git("init", "-q"); err != nil {
		return err
	}
	// Local identity, so a commit here never depends on — or is attributed
	// to — whatever global git config the machine happens to carry.
	if _, err := r.git("config", "user.name", "Factor"); err != nil {
		return err
	}
	if _, err := r.git("config", "user.email", "factor@localhost"); err != nil {
		return err
	}
	if err := r.writeIgnore(); err != nil {
		return err
	}
	r.Commit("the workspace as it stands")
	return nil
}

func (r *Repo) writeIgnore() error {
	path := filepath.Join(r.dir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil // the user's own, left alone
	}
	return os.WriteFile(path, []byte(ignore), 0o644)
}

// Commit records whatever has changed, under a message naming why.
//
// It is best-effort by design. A history is worth having and never worth
// failing a tool call for: a skill that was written but not committed is a
// skill that was written.
func (r *Repo) Commit(message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.git("add", "-A"); err != nil {
		slog.Warn("could not stage the workspace", "error", err)
		return
	}
	// Nothing staged is the ordinary case — a tool that rewrote a file with
	// the same bytes — and not something to log about.
	if out, err := r.git("diff", "--cached", "--quiet"); err == nil && out == "" {
		return
	}
	if _, err := r.git("commit", "-q", "-m", message); err != nil {
		slog.Warn("could not commit the workspace", "message", message, "error", err)
	}
}

// Committer returns a commit function tagged with a prefix, for handing to a
// tool that knows what it changed but nothing about git.
func (r *Repo) Committer(prefix string) func(string) {
	if r == nil {
		return nil
	}
	return func(what string) { r.Commit(prefix + ": " + what) }
}

func (r *Repo) git(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	// A repository this process created must not be steered by the ambient
	// environment: no global config, no hooks somebody else installed, no
	// editor waiting for input that will never come.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
