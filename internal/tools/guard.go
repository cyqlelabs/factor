package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PathGuard enforces the workspace restriction for file access. It is a
// guardrail against accidents and prompt-injected mischief, not a sandbox.
type PathGuard struct {
	workspace  string
	restrict   bool
	allowRead  bool // reads may leave the workspace
	allowPaths []string
}

func NewPathGuard(workspace string, restrict, allowReadOutside bool, allowPaths []string) *PathGuard {
	var cleaned []string
	for _, p := range allowPaths {
		if a := canonical(p); a != "" {
			cleaned = append(cleaned, a)
		}
	}
	return &PathGuard{workspace: canonical(workspace), restrict: restrict, allowRead: allowReadOutside, allowPaths: cleaned}
}

// canonical is the spelling every check compares against: absolute, and run
// through the same symlink resolution a requested path gets.
//
// Resolving one side only is a guard that refuses everything. Every request
// arrives canonicalised — and on macOS that rewrites /tmp to /private/tmp and
// /var to /private/var, on Fedora Silverblue /home to /var/home, on Windows
// the drive letter, the case of every component and any 8.3 name — while the
// root it is compared against kept whatever spelling the config happened to
// use. The prefix then never matched and every read and write was denied with
// the resolved path and the configured one printed side by side, which is a
// confusing way to say the two were never going to agree.
func canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	// A workspace that does not exist yet is still a valid root: it is created
	// on first use, and until then its unresolved form is the best answer.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func (g *PathGuard) Workspace() string { return g.workspace }

// Resolve makes a path absolute (relative paths are workspace-relative) and
// normalizes symlinks in the existing portion so links cannot escape.
func (g *PathGuard) Resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(g.workspace, path)
	}
	path = filepath.Clean(path)
	// Resolve symlinks on the deepest existing ancestor to prevent link escapes.
	probe := path
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			return filepath.Join(resolved, strings.TrimPrefix(path, probe)), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return path, nil
		}
		probe = parent
	}
}

func (g *PathGuard) inside(path string) bool {
	if under(path, g.workspace) {
		return true
	}
	for _, allow := range g.allowPaths {
		if under(path, allow) {
			return true
		}
	}
	return false
}

// under reports whether path is root or sits inside it.
//
// The comparison ignores case where the filesystem does. Windows and macOS
// both hand back one file for two spellings, so reading them as two paths
// refuses a workspace the user typed with a capital letter — and on the
// platform whose own resolver rewrites case (Windows), it also refuses a root
// that was merely written down differently from how it sits on disk.
func under(path, root string) bool {
	if root == "" {
		return false
	}
	if caseFolding {
		path, root = strings.ToLower(path), strings.ToLower(root)
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// caseFolding says whether this platform's filesystem treats two spellings of
// a name as one file. It is the default on both desktops; a case-sensitive
// volume on either only makes the guard slightly more permissive than the
// filesystem, which is the direction a guardrail against accidents can afford.
var caseFolding = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// CheckRead resolves and authorizes a read.
func (g *PathGuard) CheckRead(path string) (string, error) {
	resolved, err := g.Resolve(path)
	if err != nil {
		return "", err
	}
	if g.restrict && !g.allowRead && !g.inside(resolved) {
		return "", fmt.Errorf("read outside workspace denied: %s (workspace: %s)", resolved, g.workspace)
	}
	return resolved, nil
}

// CheckWrite resolves and authorizes a write.
func (g *PathGuard) CheckWrite(path string) (string, error) {
	resolved, err := g.Resolve(path)
	if err != nil {
		return "", err
	}
	if g.restrict && !g.inside(resolved) {
		return "", fmt.Errorf("write outside workspace denied: %s (workspace: %s)", resolved, g.workspace)
	}
	return resolved, nil
}
