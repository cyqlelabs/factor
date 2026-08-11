package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- read_file ---

func TestReadFileMissingFile(t *testing.T) {
	g := testGuard(t)
	res := fsTool(t, g, "read_file").Execute(context.Background(), map[string]any{"path": "absent.txt"})
	if !res.IsError {
		t.Fatalf("reading a missing file succeeded: %+v", res)
	}
	if !strings.Contains(res.ForLLM, "no such file") {
		t.Errorf("error = %q, want it to name the missing file", res.ForLLM)
	}
}

func TestReadFileOnADirectory(t *testing.T) {
	g := testGuard(t)
	if err := os.Mkdir(filepath.Join(g.Workspace(), "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := fsTool(t, g, "read_file").Execute(context.Background(), map[string]any{"path": "adir"})
	if !res.IsError {
		t.Errorf("reading a directory succeeded: %+v", res)
	}
}

func TestReadFileTruncatesAboveMaxReadBytes(t *testing.T) {
	g := testGuard(t)
	body := bytes.Repeat([]byte("a"), maxReadBytes+512)
	if err := os.WriteFile(filepath.Join(g.Workspace(), "long.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	res := fsTool(t, g, "read_file").Execute(context.Background(), map[string]any{"path": "long.txt"})
	if res.IsError {
		t.Fatalf("read = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "[truncated,") || !strings.Contains(res.ForLLM, "use offset/limit") {
		t.Errorf("no truncation notice in output ending %q", tailOf(res.ForLLM, 80))
	}
	if !strings.HasPrefix(res.ForLLM, strings.Repeat("a", 1000)) {
		t.Error("the head of the file was not preserved")
	}
	if len(res.ForLLM) > maxReadBytes+200 {
		t.Errorf("returned %d bytes, want it capped near %d", len(res.ForLLM), maxReadBytes)
	}
}

func TestReadFileCapsAtFourMegabytes(t *testing.T) {
	g := testGuard(t)
	const readCap = 4 << 20
	// A short first line followed by enough padding to blow past the 4MB cap.
	body := append([]byte("first line\n"), bytes.Repeat([]byte("x"), readCap)...)
	if err := os.WriteFile(filepath.Join(g.Workspace(), "huge.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// limit=1 keeps the returned slice small, so the 4MB notice is what surfaces.
	res := fsTool(t, g, "read_file").Execute(context.Background(), map[string]any{"path": "huge.txt", "limit": 1.0})
	if res.IsError {
		t.Fatalf("read = %+v", res)
	}
	if !strings.HasPrefix(res.ForLLM, "first line") {
		t.Errorf("content = %q, want it to start with the first line", tailOf(res.ForLLM, 80))
	}
	if !strings.Contains(res.ForLLM, "file larger than 4MB") {
		t.Errorf("no 4MB notice in %q", res.ForLLM)
	}
}

func TestReadFileOffsetBeyondEOF(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "l.txt", "content": "l1\nl2\nl3"})
	res := fsTool(t, g, "read_file").Execute(ctx, map[string]any{"path": "l.txt", "offset": 99.0})
	if res.IsError {
		t.Fatalf("read = %+v", res)
	}
	if res.ForLLM != "" {
		t.Errorf("content = %q, want empty for an offset past EOF", res.ForLLM)
	}
}

func TestReadFileLimitBeyondEOFReturnsRemainder(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "l.txt", "content": "l1\nl2\nl3"})
	res := fsTool(t, g, "read_file").Execute(ctx, map[string]any{"path": "l.txt", "offset": 2.0, "limit": 500.0})
	if res.ForLLM != "l2\nl3" {
		t.Errorf("content = %q, want the remainder from line 2", res.ForLLM)
	}
}

func TestReadFileOffsetOneIsTheFirstLine(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "l.txt", "content": "l1\nl2\nl3"})
	res := fsTool(t, g, "read_file").Execute(ctx, map[string]any{"path": "l.txt", "offset": 1.0, "limit": 1.0})
	if res.ForLLM != "l1" {
		t.Errorf("content = %q, want the 1-based first line", res.ForLLM)
	}
}

func TestReadFileEmptyFile(t *testing.T) {
	g := testGuard(t)
	if err := os.WriteFile(filepath.Join(g.Workspace(), "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := fsTool(t, g, "read_file").Execute(context.Background(), map[string]any{"path": "empty.txt"})
	if res.IsError || res.ForLLM != "" {
		t.Errorf("read of an empty file = %+v", res)
	}
}

// --- write_file ---

func TestWriteFileMkdirFailure(t *testing.T) {
	g := testGuard(t)
	// A regular file where a parent directory would have to go.
	if err := os.WriteFile(filepath.Join(g.Workspace(), "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := fsTool(t, g, "write_file").Execute(context.Background(), map[string]any{
		"path": "blocker/child.txt", "content": "y",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "mkdir") {
		t.Errorf("result = %+v, want a mkdir failure", res)
	}
}

func TestWriteFileOntoADirectoryFails(t *testing.T) {
	g := testGuard(t)
	if err := os.Mkdir(filepath.Join(g.Workspace(), "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := fsTool(t, g, "write_file").Execute(context.Background(), map[string]any{
		"path": "adir", "content": "y",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "write ") {
		t.Errorf("result = %+v, want a write failure", res)
	}
}

func TestWriteFileEmptyContent(t *testing.T) {
	g := testGuard(t)
	res := fsTool(t, g, "write_file").Execute(context.Background(), map[string]any{"path": "zero.txt", "content": ""})
	if res.IsError || !strings.Contains(res.ForLLM, "Wrote 0 bytes") {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(g.Workspace(), "zero.txt")); err != nil {
		t.Errorf("empty file not created: %v", err)
	}
}

func TestWriteFileOverwrites(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	w := fsTool(t, g, "write_file")
	w.Execute(ctx, map[string]any{"path": "o.txt", "content": "original content"})
	w.Execute(ctx, map[string]any{"path": "o.txt", "content": "new"})
	res := fsTool(t, g, "read_file").Execute(ctx, map[string]any{"path": "o.txt"})
	if res.ForLLM != "new" {
		t.Errorf("content after overwrite = %q, want %q", res.ForLLM, "new")
	}
}

// --- edit_file ---

func TestEditFileReplaceAll(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "r.txt", "content": "a x a x a"})
	edit := fsTool(t, g, "edit_file")

	res := edit.Execute(ctx, map[string]any{"path": "r.txt", "old_string": "a", "new_string": "b"})
	if !res.IsError || !strings.Contains(res.ForLLM, "appears 3 times") {
		t.Fatalf("ambiguous edit = %+v, want a refusal naming the match count", res)
	}

	res = edit.Execute(ctx, map[string]any{
		"path": "r.txt", "old_string": "a", "new_string": "b", "replace_all": true,
	})
	if res.IsError || !strings.Contains(res.ForLLM, "Replaced 3 occurrence") {
		t.Fatalf("replace_all edit = %+v", res)
	}
	got := fsTool(t, g, "read_file").Execute(ctx, map[string]any{"path": "r.txt"}).ForLLM
	if got != "b x b x b" {
		t.Errorf("content = %q, want %q", got, "b x b x b")
	}
}

func TestEditFileReplaceAllFalseIsStillStrict(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "r.txt", "content": "dup dup"})
	res := fsTool(t, g, "edit_file").Execute(ctx, map[string]any{
		"path": "r.txt", "old_string": "dup", "new_string": "x", "replace_all": false,
	})
	if !res.IsError {
		t.Errorf("replace_all=false accepted an ambiguous edit: %+v", res)
	}
}

func TestEditFileRejectsEmptyOldString(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "e.txt", "content": "content"})
	res := fsTool(t, g, "edit_file").Execute(ctx, map[string]any{
		"path": "e.txt", "old_string": "", "new_string": "x",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "old_string must not be empty") {
		t.Errorf("result = %+v", res)
	}
}

func TestEditFileMissingFile(t *testing.T) {
	g := testGuard(t)
	res := fsTool(t, g, "edit_file").Execute(context.Background(), map[string]any{
		"path": "gone.txt", "old_string": "a", "new_string": "b",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "no such file") {
		t.Errorf("result = %+v", res)
	}
}

func TestEditFileWriteBackFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "ro.txt", "content": "before"})
	path := filepath.Join(g.Workspace(), "ro.txt")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	res := fsTool(t, g, "edit_file").Execute(ctx, map[string]any{
		"path": "ro.txt", "old_string": "before", "new_string": "after",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "write ") {
		t.Errorf("result = %+v, want a write-back failure on a read-only file", res)
	}
	if got, _ := os.ReadFile(path); string(got) != "before" {
		t.Errorf("file content = %q, want it unchanged", got)
	}
}

func TestEditFileDeletesWhenNewStringIsEmpty(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	fsTool(t, g, "write_file").Execute(ctx, map[string]any{"path": "d.txt", "content": "keep-DROP-keep"})
	res := fsTool(t, g, "edit_file").Execute(ctx, map[string]any{
		"path": "d.txt", "old_string": "-DROP-", "new_string": "",
	})
	if res.IsError {
		t.Fatalf("edit = %+v", res)
	}
	if got := fsTool(t, g, "read_file").Execute(ctx, map[string]any{"path": "d.txt"}).ForLLM; got != "keepkeep" {
		t.Errorf("content = %q, want %q", got, "keepkeep")
	}
}

// --- list_dir ---

func TestListDirEmptyDirectory(t *testing.T) {
	g := testGuard(t)
	if err := os.Mkdir(filepath.Join(g.Workspace(), "hollow"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := fsTool(t, g, "list_dir").Execute(context.Background(), map[string]any{"path": "hollow"})
	if res.IsError || !strings.Contains(res.ForLLM, "(empty)") {
		t.Errorf("result = %+v, want an explicit empty marker", res)
	}
}

func TestListDirMissingDirectory(t *testing.T) {
	g := testGuard(t)
	res := fsTool(t, g, "list_dir").Execute(context.Background(), map[string]any{"path": "ghost"})
	if !res.IsError || !strings.Contains(res.ForLLM, "list ") {
		t.Errorf("result = %+v, want a list failure", res)
	}
}

func TestListDirDefaultsToWorkspace(t *testing.T) {
	g := testGuard(t)
	if err := os.WriteFile(filepath.Join(g.Workspace(), "root-marker.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := fsTool(t, g, "list_dir").Execute(context.Background(), map[string]any{})
	if res.IsError || !strings.Contains(res.ForLLM, "root-marker.txt") {
		t.Errorf("result = %+v, want the workspace listing", res)
	}
}

func TestListDirMarksDirectoriesAndSorts(t *testing.T) {
	g := testGuard(t)
	if err := os.Mkdir(filepath.Join(g.Workspace(), "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha.txt", "gamma.txt"} {
		if err := os.WriteFile(filepath.Join(g.Workspace(), name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := fsTool(t, g, "list_dir").Execute(context.Background(), map[string]any{})
	if res.IsError {
		t.Fatalf("list = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "beta/") {
		t.Errorf("directories are not marked with a trailing slash: %q", res.ForLLM)
	}
	a := strings.Index(res.ForLLM, "alpha.txt")
	b := strings.Index(res.ForLLM, "beta/")
	c := strings.Index(res.ForLLM, "gamma.txt")
	if a >= b || b >= c {
		t.Errorf("entries are not sorted by name: %q", res.ForLLM)
	}
}

// --- guard enforcement across every fs tool ---

func TestFSToolsRejectPathsOutsideWorkspace(t *testing.T) {
	g := testGuard(t)
	ctx := context.Background()
	calls := map[string]map[string]any{
		"read_file":  {"path": "/etc/hostname"},
		"write_file": {"path": "/etc/factor-should-not-exist", "content": "x"},
		"edit_file":  {"path": "/etc/hostname", "old_string": "a", "new_string": "b"},
		"list_dir":   {"path": "/etc"},
	}
	for name, args := range calls {
		t.Run(name, func(t *testing.T) {
			res := fsTool(t, g, name).Execute(ctx, args)
			if !res.IsError || !strings.Contains(res.ForLLM, "outside workspace denied") {
				t.Errorf("result = %+v, want a workspace denial", res)
			}
		})
	}
}

// --- exec ---

func TestExecRejectsEmptyCommand(t *testing.T) {
	et := execTool(t, testGuard(t))
	for _, command := range []string{"", "   ", "\t\n "} {
		res := et.Execute(context.Background(), map[string]any{"command": command})
		if !res.IsError || !strings.Contains(res.ForLLM, "command must not be empty") {
			t.Errorf("command %q = %+v, want an empty-command refusal", command, res)
		}
	}
}

func TestExecRunsInTheGivenWorkingDir(t *testing.T) {
	g := testGuard(t)
	sub := filepath.Join(g.Workspace(), "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	res := execTool(t, g).Execute(context.Background(), map[string]any{"command": "pwd", "working_dir": "sub"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(res.ForLLM))
	if err != nil {
		t.Fatalf("pwd output %q is not a real path: %v", res.ForLLM, err)
	}
	want, err := filepath.EvalSymlinks(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

func TestExecRejectsWorkingDirOutsideWorkspace(t *testing.T) {
	res := execTool(t, testGuard(t)).Execute(context.Background(), map[string]any{
		"command": "pwd", "working_dir": "/etc",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "outside workspace denied") {
		t.Errorf("result = %+v, want a workspace denial", res)
	}
}

func TestExecTimeoutSecsOverridesTheDefault(t *testing.T) {
	g := testGuard(t)
	// A default so small that nothing can complete under it.
	et, err := NewExecTool(g, time.Nanosecond, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := et.Execute(context.Background(), map[string]any{"command": "echo ok"})
	if !res.IsError || !strings.Contains(res.ForLLM, "timed out") {
		t.Fatalf("baseline = %+v, want the tiny default timeout to fire", res)
	}
	res = et.Execute(context.Background(), map[string]any{"command": "echo ok", "timeout_secs": 30.0})
	if res.IsError || !strings.Contains(res.ForLLM, "ok") {
		t.Errorf("with timeout_secs = %+v, want the override to replace the default", res)
	}
}

func TestExecIgnoresNonPositiveTimeoutSecs(t *testing.T) {
	g := testGuard(t)
	et, err := NewExecTool(g, 5*time.Second, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := et.Execute(context.Background(), map[string]any{"command": "echo ok", "timeout_secs": 0.0})
	if res.IsError || !strings.Contains(res.ForLLM, "ok") {
		t.Errorf("result = %+v, want timeout_secs=0 to keep the configured default", res)
	}
}

func TestExecTruncatesLargeOutput(t *testing.T) {
	g := testGuard(t)
	et := execTool(t, g)
	const size = 40 * 1024
	if err := os.WriteFile(filepath.Join(g.Workspace(), "big.txt"), bytes.Repeat([]byte("z"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	res := et.Execute(context.Background(), map[string]any{"command": "cat big.txt"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "bytes truncated") {
		t.Errorf("no truncation marker in a %d byte output", size)
	}
	if len(res.ForLLM) >= size {
		t.Errorf("output length %d, want it capped well below %d", len(res.ForLLM), size)
	}
	if !strings.HasPrefix(res.ForLLM, "zzzz") || !strings.HasSuffix(res.ForLLM, "zzzz") {
		t.Error("truncation should keep both the head and the tail")
	}
}

func TestExecReportsNoOutputForSilentCommands(t *testing.T) {
	res := execTool(t, testGuard(t)).Execute(context.Background(), map[string]any{"command": "true"})
	if res.IsError || res.ForLLM != "(no output)" {
		t.Errorf("result = %+v, want the (no output) placeholder", res)
	}
}

func TestExecCapturesStderr(t *testing.T) {
	res := execTool(t, testGuard(t)).Execute(context.Background(), map[string]any{
		"command": "echo to-stderr >&2; exit 1",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "to-stderr") {
		t.Errorf("result = %+v, want stderr folded into the combined output", res)
	}
}

func TestNewExecToolWithoutDefaultDenyList(t *testing.T) {
	g := testGuard(t)
	et, err := NewExecTool(g, 0, false, []string{`\bformat-the-drive\b`})
	if err != nil {
		t.Fatal(err)
	}
	if et.timeout != 2*time.Minute {
		t.Errorf("timeout = %s, want the 2m fallback for a non-positive timeout", et.timeout)
	}

	res := et.Execute(context.Background(), map[string]any{"command": "format-the-drive now"})
	if !res.IsError || !strings.Contains(res.ForLLM, "blocked by safety pattern") {
		t.Errorf("custom deny pattern = %+v, want a block", res)
	}
	// With the defaults off, only the custom list applies (echo keeps this inert).
	res = et.Execute(context.Background(), map[string]any{"command": "echo rm -rf /"})
	if res.IsError {
		t.Errorf("result = %+v, want the built-in patterns disabled when enableDeny is false", res)
	}
}

func TestNewExecToolCustomPatternsExtendTheDefaults(t *testing.T) {
	et, err := NewExecTool(testGuard(t), time.Second, true, []string{`\bnever-run-me\b`})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"never-run-me please", "shutdown now"} {
		res := et.Execute(context.Background(), map[string]any{"command": command})
		if !res.IsError || !strings.Contains(res.ForLLM, "blocked") {
			t.Errorf("command %q = %+v, want a block", command, res)
		}
	}
}

func TestNewExecToolRejectsInvalidDenyPattern(t *testing.T) {
	g := testGuard(t)
	for _, enableDefaults := range []bool{false, true} {
		_, err := NewExecTool(g, time.Second, enableDefaults, []string{"[unclosed"})
		if err == nil {
			t.Fatalf("enableDeny=%v: invalid regex was accepted", enableDefaults)
		}
		if !strings.Contains(err.Error(), "bad deny pattern") {
			t.Errorf("enableDeny=%v: error = %v, want it to name the bad pattern", enableDefaults, err)
		}
	}
}

func TestExecHonorsCallerContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := execTool(t, testGuard(t)).Execute(ctx, map[string]any{"command": "echo hi"})
	if !res.IsError {
		t.Errorf("result = %+v, want a cancelled context to abort the command", res)
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
