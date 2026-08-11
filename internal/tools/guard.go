package tools

import (
	"fmt"
	"os"
	"path/filepath"
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
	abs, err := filepath.Abs(workspace)
	if err == nil {
		workspace = abs
	}
	var cleaned []string
	for _, p := range allowPaths {
		if a, err := filepath.Abs(p); err == nil {
			cleaned = append(cleaned, a)
		}
	}
	return &PathGuard{workspace: workspace, restrict: restrict, allowRead: allowReadOutside, allowPaths: cleaned}
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
	if path == g.workspace || strings.HasPrefix(path, g.workspace+string(filepath.Separator)) {
		return true
	}
	for _, allow := range g.allowPaths {
		if path == allow || strings.HasPrefix(path, allow+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

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
