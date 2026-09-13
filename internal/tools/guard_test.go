package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// The root and the request have to be spelled the same way before they can be
// compared. A request is always resolved; a root that was not is how a guard
// refuses every path in its own workspace.
func TestGuardResolvesItsOwnRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := NewPathGuard(link, true, false, nil)
	if _, err := g.CheckRead(filepath.Join(link, "notes.md")); err != nil {
		t.Fatalf("a file in the workspace was refused: %v", err)
	}
	if _, err := g.CheckWrite("new.md"); err != nil {
		t.Fatalf("a workspace-relative write was refused: %v", err)
	}
}

// An allow_paths entry gets the same treatment: on macOS every /tmp path
// resolves to /private/tmp, so an unresolved entry there allows nothing.
func TestGuardResolvesAllowPaths(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "shared")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "data.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := NewPathGuard(t.TempDir(), true, false, []string{link})
	if _, err := g.CheckRead(filepath.Join(link, "data.csv")); err != nil {
		t.Fatalf("an allowed path was refused: %v", err)
	}
}

// A workspace that does not exist yet is still the root, and a guard must not
// refuse the very writes that would create it.
func TestGuardAcceptsARootThatIsNotThereYet(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not", "made", "yet")
	g := NewPathGuard(root, true, false, nil)
	if _, err := g.CheckWrite(filepath.Join(root, "AGENT.md")); err != nil {
		t.Fatalf("writing into a workspace about to be created was refused: %v", err)
	}
	if _, err := g.CheckWrite(filepath.Join(t.TempDir(), "elsewhere.md")); err == nil {
		t.Fatal("a write outside it should still be refused")
	}
}

func TestUnderFoldsCaseOnlyWhereTheFilesystemDoes(t *testing.T) {
	old := caseFolding
	t.Cleanup(func() { caseFolding = old })

	root := filepath.Join(string(filepath.Separator), "Users", "Nico", "ws")
	inner := filepath.Join(root, "notes.md")
	shouted := filepath.Join(string(filepath.Separator), "users", "nico", "WS", "notes.md")

	caseFolding = true
	if !under(inner, root) || !under(shouted, root) {
		t.Error("a case-insensitive filesystem hands back one file for both spellings")
	}
	caseFolding = false
	if !under(inner, root) || under(shouted, root) {
		t.Error("a case-sensitive filesystem has two different paths here")
	}
	if under(inner, "") {
		t.Error("an empty root contains nothing")
	}
	if under(filepath.Join(string(filepath.Separator), "Users", "Nico", "ws-other"), root) {
		t.Error("a sibling whose name starts the same is not inside")
	}
}
