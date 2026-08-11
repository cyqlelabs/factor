package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hermeticGit points git at a throwaway global config and skips the test when
// git is unavailable. The returned path is that config, which localSkillRepo
// appends url rewrites to so "remote" clones resolve to a local repository and
// the test never touches the network.
func hermeticGit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	base := "[user]\n\tname = Factor Test\n\temail = test@example.invalid\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(cfg, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	return cfg
}

func runGit(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// rewriteURL makes git resolve url to target instead of dialing out.
func rewriteURL(t *testing.T, cfg, url, target string) {
	t.Helper()
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "[url \"file://%s\"]\n\tinsteadOf = %s\n", target, url); err != nil {
		t.Fatal(err)
	}
}

// localSkillRepo builds a bare repository holding one SKILL.md and returns the
// URL that resolves to it.
func localSkillRepo(t *testing.T, cfg, skill string) string {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + skill + "\ndescription: cloned from git\n---\n\nSkill body.\n"
	if err := os.WriteFile(filepath.Join(work, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "-q", work)
	runGit(t, "-C", work, "add", "SKILL.md")
	runGit(t, "-C", work, "commit", "-q", "-m", "add skill")

	bare := filepath.Join(base, skill+".git")
	runGit(t, "clone", "-q", "--bare", work, bare)

	url := "https://git.invalid/" + skill + ".git"
	rewriteURL(t, cfg, url, bare)
	return url
}

func TestInstallToolDescriptor(t *testing.T) {
	tool := &InstallTool{}
	if tool.Name() != "skill_install" {
		t.Errorf("Name() = %q, want skill_install", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() is empty; the model has nothing to select on")
	}

	params := tool.Parameters()
	if params["type"] != "object" {
		t.Errorf("Parameters()[type] = %v, want object", params["type"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters()[properties] = %T, want map[string]any", params["properties"])
	}
	for _, key := range []string{"source", "name"} {
		if _, ok := props[key]; !ok {
			t.Errorf("Parameters() omits property %q", key)
		}
	}
	required, ok := params["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "source" {
		t.Errorf("Parameters()[required] = %v, want [source]", params["required"])
	}
}

func TestInstallToolRejectsNonDirectorySource(t *testing.T) {
	file := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(file, []byte("---\nname: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &InstallTool{Root: t.TempDir()}

	res := tool.Execute(context.Background(), map[string]any{"source": file, "name": "fromfile"})
	if !res.IsError || !strings.Contains(res.ForLLM, "not a directory") {
		t.Errorf("installing from a regular file = %+v", res)
	}
	res = tool.Execute(context.Background(), map[string]any{"source": filepath.Join(t.TempDir(), "nope"), "name": "missing"})
	if !res.IsError {
		t.Errorf("installing from a missing path = %+v", res)
	}
}

func TestInstallToolReportsCopyFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &InstallTool{Root: filepath.Join(blocker, "skills")}
	res := tool.Execute(context.Background(), map[string]any{"source": src, "name": "blocked"})
	if !res.IsError || !strings.Contains(res.ForLLM, "copy failed") {
		t.Errorf("install into an uncreatable root = %+v", res)
	}
}

func TestInstallToolDefaultsNameToSourceBasename(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tidy-inbox")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: tidy\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	res := (&InstallTool{Root: root}).Execute(context.Background(), map[string]any{"source": src})
	if res.IsError {
		t.Fatalf("install: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "tidy-inbox", "SKILL.md")); err != nil {
		t.Errorf("skill not installed under the source basename: %v", err)
	}
}

func TestInstallToolClonesLocalGitRepo(t *testing.T) {
	cfg := hermeticGit(t)
	url := localSkillRepo(t, cfg, "fromgit")
	root := t.TempDir()

	// name omitted: it is derived from the source basename minus ".git"
	res := (&InstallTool{Root: root}).Execute(context.Background(), map[string]any{"source": url})
	if res.IsError {
		t.Fatalf("clone install: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "fromgit", "SKILL.md")); err != nil {
		t.Fatalf("cloned skill has no SKILL.md: %v", err)
	}

	list := NewLoader(root).List()
	if len(list) != 1 || list[0].Name != "fromgit" || list[0].Description != "cloned from git" {
		t.Errorf("cloned skill not loadable: %+v", list)
	}
}

func TestInstallToolReportsCloneFailure(t *testing.T) {
	cfg := hermeticGit(t)
	url := "https://git.invalid/missing.git"
	rewriteURL(t, cfg, url, filepath.Join(t.TempDir(), "no-such-repo"))
	root := t.TempDir()

	res := (&InstallTool{Root: root}).Execute(context.Background(), map[string]any{"source": url})
	if !res.IsError || !strings.Contains(res.ForLLM, "git clone failed") {
		t.Fatalf("clone of a missing repository = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Error("a failed clone left a directory behind")
	}
}

func TestInstallToolRejectsClonedRepoWithoutSkillFile(t *testing.T) {
	cfg := hermeticGit(t)

	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("no skill here"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, "init", "-q", work)
	runGit(t, "-C", work, "add", "README.md")
	runGit(t, "-C", work, "commit", "-q", "-m", "readme only")
	bare := filepath.Join(base, "notaskill.git")
	runGit(t, "clone", "-q", "--bare", work, bare)

	url := "https://git.invalid/notaskill.git"
	rewriteURL(t, cfg, url, bare)
	root := t.TempDir()

	res := (&InstallTool{Root: root}).Execute(context.Background(), map[string]any{"source": url})
	if !res.IsError || !strings.Contains(res.ForLLM, "not a skill") {
		t.Fatalf("repository without SKILL.md = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "notaskill")); !os.IsNotExist(err) {
		t.Error("rejected clone was not cleaned up")
	}
}
