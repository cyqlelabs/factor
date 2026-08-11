package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxReadBytes = 256 * 1024

// NewFSTools returns the file tools bound to one path guard.
func NewFSTools(guard *PathGuard) []Tool {
	return []Tool{
		&readFileTool{guard},
		&writeFileTool{guard},
		&editFileTool{guard},
		&listDirTool{guard},
	}
}

type readFileTool struct{ guard *PathGuard }

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read a file. Relative paths are workspace-relative. Use offset/limit (1-based line numbers) for large files."
}
func (t *readFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "File path"},
			"offset": map[string]any{"type": "integer", "description": "First line to read (1-based)"},
			"limit":  map[string]any{"type": "integer", "description": "Max lines to read"},
		},
		"required": []any{"path"},
	}
}

func (t *readFileTool) Execute(_ context.Context, args map[string]any) *Result {
	path, err := t.guard.CheckRead(StringArg(args, "path"))
	if err != nil {
		return Errorf("%v", err)
	}
	// Bounded read: never slurp a multi-GB file into memory.
	f, err := os.Open(path)
	if err != nil {
		return Errorf("read %s: %v", path, err)
	}
	defer f.Close()
	const readCap = 4 << 20
	data, err := io.ReadAll(io.LimitReader(f, readCap+1))
	if err != nil {
		return Errorf("read %s: %v", path, err)
	}
	capped := len(data) > readCap
	if capped {
		data = data[:readCap]
	}
	content := string(data)
	offset, limit := IntArg(args, "offset", 0), IntArg(args, "limit", 0)
	if offset > 0 || limit > 0 {
		lines := strings.Split(content, "\n")
		start := 0
		if offset > 1 {
			start = min(offset-1, len(lines))
		}
		end := len(lines)
		if limit > 0 {
			end = min(start+limit, len(lines))
		}
		content = strings.Join(lines[start:end], "\n")
	}
	if len(content) > maxReadBytes {
		content = content[:maxReadBytes] + fmt.Sprintf("\n... [truncated, %d bytes read; use offset/limit]", len(data))
	} else if capped {
		content += "\n... [file larger than 4MB; only the first 4MB was read]"
	}
	return Text(content)
}

type writeFileTool struct{ guard *PathGuard }

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Write content to a file, creating parent directories. Overwrites existing files — read first when editing."
}
func (t *writeFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
		},
		"required": []any{"path", "content"},
	}
}

func (t *writeFileTool) Execute(_ context.Context, args map[string]any) *Result {
	path, err := t.guard.CheckWrite(StringArg(args, "path"))
	if err != nil {
		return Errorf("%v", err)
	}
	content := StringArg(args, "content")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Errorf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Errorf("write %s: %v", path, err)
	}
	return Textf("Wrote %d bytes to %s", len(content), path)
}

type editFileTool struct{ guard *PathGuard }

func (t *editFileTool) Name() string { return "edit_file" }
func (t *editFileTool) Description() string {
	return "Replace old_string with new_string in a file. old_string must match exactly once unless replace_all is true."
}
func (t *editFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":        map[string]any{"type": "string"},
			"old_string":  map[string]any{"type": "string"},
			"new_string":  map[string]any{"type": "string"},
			"replace_all": map[string]any{"type": "boolean"},
		},
		"required": []any{"path", "old_string", "new_string"},
	}
}

func (t *editFileTool) Execute(_ context.Context, args map[string]any) *Result {
	path, err := t.guard.CheckWrite(StringArg(args, "path"))
	if err != nil {
		return Errorf("%v", err)
	}
	oldS, newS := StringArg(args, "old_string"), StringArg(args, "new_string")
	if oldS == "" {
		return Errorf("old_string must not be empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Errorf("read %s: %v", path, err)
	}
	content := string(data)
	count := strings.Count(content, oldS)
	switch {
	case count == 0:
		return Errorf("old_string not found in %s", path)
	case count > 1 && !BoolArg(args, "replace_all", false):
		return Errorf("old_string appears %d times in %s; make it unique or set replace_all", count, path)
	}
	content = strings.ReplaceAll(content, oldS, newS)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Errorf("write %s: %v", path, err)
	}
	return Textf("Replaced %d occurrence(s) in %s", count, path)
}

type listDirTool struct{ guard *PathGuard }

func (t *listDirTool) Name() string { return "list_dir" }
func (t *listDirTool) Description() string {
	return "List a directory. Defaults to the workspace root."
}
func (t *listDirTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
	}
}

func (t *listDirTool) Execute(_ context.Context, args map[string]any) *Result {
	path := StringArg(args, "path")
	if path == "" {
		path = t.guard.Workspace()
	}
	resolved, err := t.guard.CheckRead(path)
	if err != nil {
		return Errorf("%v", err)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return Errorf("list %s: %v", resolved, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", resolved)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		fmt.Fprintf(&b, "  %s\n", name)
	}
	if len(entries) == 0 {
		b.WriteString("  (empty)\n")
	}
	return Text(b.String())
}
