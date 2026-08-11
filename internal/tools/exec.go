package tools

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Catastrophic-action patterns. This is a guardrail against obvious
// disasters, not a security boundary; package installs and normal system
// administration stay allowed on purpose (Factor is a desktop companion).
var defaultDenyPatterns = []string{
	`\brm\s+(-[a-zA-Z]*\s+)*(/|/\*|~|\$HOME)\s*$`,
	`\brm\s+-[a-zA-Z]*r[a-zA-Z]*f`,
	`\bmkfs\b`,
	`\bdd\b.*\bof=/dev/(sd|nvme|mmcblk|vd|hd)`,
	`>\s*/dev/(sd|nvme|mmcblk|vd|hd)`,
	`:\(\)\s*\{.*\};\s*:`,
	`\b(shutdown|reboot|poweroff|halt)\b`,
	`\bkill\s+-9\s+-1\b`,
	`\bchmod\s+(-[a-zA-Z]*\s+)*777\s+/`,
	`\bcurl\b[^|]*\|\s*(ba|z|da)?sh\b`,
	`\bwget\b[^|]*\|\s*(ba|z|da)?sh\b`,
	`\bhistory\s+-c\b`,
}

type ExecTool struct {
	guard          *PathGuard
	timeout        time.Duration
	denyPatterns   []*regexp.Regexp
	maxOutputBytes int
}

func NewExecTool(guard *PathGuard, timeout time.Duration, enableDeny bool, customDeny []string) (*ExecTool, error) {
	t := &ExecTool{guard: guard, timeout: timeout, maxOutputBytes: 32 * 1024}
	if t.timeout <= 0 {
		t.timeout = 2 * time.Minute
	}
	patterns := customDeny
	if enableDeny {
		patterns = append(append([]string{}, defaultDenyPatterns...), customDeny...)
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("bad deny pattern %q: %w", p, err)
		}
		t.denyPatterns = append(t.denyPatterns, re)
	}
	return t, nil
}

func (t *ExecTool) Name() string { return "exec" }
func (t *ExecTool) Description() string {
	return "Run a shell command (sh -c) in the workspace. Returns combined output and exit code. Long or interactive commands will be killed at the timeout."
}
func (t *ExecTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":      map[string]any{"type": "string", "description": "Shell command to run"},
			"working_dir":  map[string]any{"type": "string", "description": "Working directory (defaults to workspace)"},
			"timeout_secs": map[string]any{"type": "integer", "description": "Override the default timeout"},
		},
		"required": []any{"command"},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]any) *Result {
	command := strings.TrimSpace(StringArg(args, "command"))
	if command == "" {
		return Errorf("command must not be empty")
	}
	for _, re := range t.denyPatterns {
		if re.MatchString(command) {
			return Errorf("command blocked by safety pattern %q — if this is intentional, the user can adjust tools.custom_deny_patterns or run it themselves", re.String())
		}
	}

	dir := t.guard.Workspace()
	if wd := StringArg(args, "working_dir"); wd != "" {
		resolved, err := t.guard.CheckRead(wd)
		if err != nil {
			return Errorf("%v", err)
		}
		dir = resolved
	}

	timeout := t.timeout
	if secs := IntArg(args, "timeout_secs", 0); secs > 0 {
		timeout = time.Duration(secs) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	// Without these, a killed sh can leave grandchildren holding the output
	// pipe and CombinedOutput would block long past the timeout.
	cmd.WaitDelay = 2 * time.Second
	setProcessGroup(cmd)
	out, err := cmd.CombinedOutput()

	text := string(out)
	if len(text) > t.maxOutputBytes {
		half := t.maxOutputBytes / 2
		text = text[:half] + fmt.Sprintf("\n... [%d bytes truncated] ...\n", len(text)-t.maxOutputBytes) + text[len(text)-half:]
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return Errorf("command timed out after %s\n%s", timeout, text)
	case err != nil:
		if exitErr, ok := err.(*exec.ExitError); ok {
			return &Result{ForLLM: fmt.Sprintf("exit code %d\n%s", exitErr.ExitCode(), text), IsError: true}
		}
		return Errorf("exec failed: %v\n%s", err, text)
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return Text(text)
}
