package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func newRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("be helpful\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Open(dir, true)
	if r == nil {
		t.Fatal("Open returned nothing with versioning on")
	}
	return r, dir
}

func log(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "log", "--pretty=%s")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, out)
	}
	return string(out)
}

// Off is the default, and off must create nothing: a repository in somebody's
// home directory is not a thing to inherit from an upgrade.
func TestDisabledCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if r := Open(dir, false); r != nil {
		t.Error("versioning off should produce no repository")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Error("a .git was created with versioning off")
	}
}

// Every method has to be safe on a nil repo: that is what "off" is.
func TestNilRepoIsSafe(t *testing.T) {
	var r *Repo
	r.Commit("nothing")
	if got := r.Committer("skill"); got != nil {
		t.Error("a nil repo should hand out no committer")
	}
}

func TestOpenCommitsTheStartingState(t *testing.T) {
	_, dir := newRepo(t)
	if !strings.Contains(log(t, dir), "the workspace as it stands") {
		t.Errorf("no baseline commit:\n%s", log(t, dir))
	}
}

func TestCommitRecordsAChange(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("be terse\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Commit("skill: updated tone")

	got := log(t, dir)
	if !strings.Contains(got, "skill: updated tone") {
		t.Errorf("change not recorded:\n%s", got)
	}
}

// A tool that rewrote a file with the same bytes changed nothing, and an
// empty commit for it is noise in the one history meant to be readable.
func TestCommitWithNothingChangedIsANoOp(t *testing.T) {
	r, dir := newRepo(t)
	before := log(t, dir)
	r.Commit("skill: wrote the same thing again")
	if log(t, dir) != before {
		t.Errorf("an empty commit was made:\n%s", log(t, dir))
	}
}

// Session transcripts churn on every message and would bury the changes worth
// reading.
func TestSessionsAreNotVersioned(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", "cli-x.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Commit("skill: something else")

	cmd := exec.Command("git", "ls-files")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "sessions/") {
		t.Errorf("session transcripts were versioned:\n%s", out)
	}
}

func TestCommitterPrefixesTheMessage(t *testing.T) {
	r, dir := newRepo(t)
	commit := r.Committer("skill")
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit("created deploy-notes")
	if !strings.Contains(log(t, dir), "skill: created deploy-notes") {
		t.Errorf("log = %s", log(t, dir))
	}
}

// A workspace already under the user's own git is adopted, not re-initialised,
// and their .gitignore is left alone.
func TestExistingRepositoryIsAdopted(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.name", "Someone"}, {"config", "user.email", "someone@example.test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if r := Open(dir, true); r == nil {
		t.Fatal("an existing repository should be adopted")
	}
	got, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "mine\n" {
		t.Errorf(".gitignore was overwritten: %q", got)
	}
	// Adoption must not have made a baseline commit into somebody else's
	// history without being asked.
	cmd := exec.Command("git", "log", "--oneline")
	cmd.Dir = dir
	if out, _ := cmd.CombinedOutput(); strings.Contains(string(out), "the workspace as it stands") {
		t.Errorf("adopted repository was given a baseline commit:\n%s", out)
	}
}

// git missing means no history and no complaint: the workspace works exactly
// as it did before, and startup is not something to fail over a missing diff.
func TestNoGitMeansNoRepository(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if r := Open(t.TempDir(), true); r != nil {
		t.Error("versioning should be off when git is not installed")
	}
}

func TestEmptyDirectoryIsOff(t *testing.T) {
	if r := Open("", true); r != nil {
		t.Error("no workspace means no repository")
	}
}

// A directory git cannot initialise disables versioning rather than failing.
func TestUninitialisableWorkspaceIsOff(t *testing.T) {
	requireGit(t)
	if r := Open(filepath.Join(t.TempDir(), "does", "not", "exist"), true); r != nil {
		t.Error("an uninitialisable workspace should produce no repository")
	}
}

// Committing into a repository that has gone away must warn rather than panic:
// the workspace can be moved or cleared under a running gateway.
func TestCommitAfterTheRepositoryDisappears(t *testing.T) {
	r, dir := newRepo(t)
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	r.Commit("skill: written after the history was deleted")
}

func TestGitIgnoreIsWrittenOnce(t *testing.T) {
	r, dir := newRepo(t)
	before, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "sessions/") {
		t.Errorf(".gitignore = %q", before)
	}
	// Reopening must not rewrite what is already there.
	if again := Open(dir, true); again == nil {
		t.Fatal("reopening an initialised workspace should work")
	}
	after, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if string(after) != string(before) {
		t.Error(".gitignore was rewritten on reopen")
	}
	_ = r
}
