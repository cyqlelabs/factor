package tools

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Catastrophic-action patterns. This is a guardrail against obvious
// disasters, not a security boundary; package installs and normal system
// administration stay allowed on purpose (Factor is a desktop companion).
var defaultDenyPatterns = []string{
	`\brm\s+(-[a-zA-Z]*\s+)*(/|/\*|~|\$HOME)\s*$`,
	// recursive+force in any spelling/order: -rf, -Rf, -fr, -r ... -f, -f ... -r
	`\brm\s+(\S+\s+)*-[a-zA-Z]*([rR][a-zA-Z]*[fF]|[fF][a-zA-Z]*[rR])`,
	`\brm\s+(\S+\s+)*-[a-zA-Z]*[rR][a-zA-Z]*(\s+\S+)*\s+-[a-zA-Z]*[fF]`,
	`\brm\s+(\S+\s+)*-[a-zA-Z]*[fF][a-zA-Z]*(\s+\S+)*\s+-[a-zA-Z]*[rR]\b`,
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

// CommandGuard is the deny-list every path that runs a shell command is
// checked against. It exists as its own type because there is more than one
// such path: the exec tool runs a command in the foreground and job_start
// runs one in the background, and for a while only the first was checked —
// so a command the user had denied ran anyway by being asked for as a job.
// Delegating slow work is ordinary here, and a rail one of two first-party
// shells enforces is not a rail.
//
// A nil guard passes everything, which is what a configuration with the
// defaults switched off and no custom patterns means.
type CommandGuard struct {
	patterns []*regexp.Regexp
}

// NewCommandGuard compiles the deny-list: the built-in catastrophes when
// enableDeny is set, plus the user's own patterns either way.
func NewCommandGuard(enableDeny bool, customDeny []string) (*CommandGuard, error) {
	g := &CommandGuard{}
	patterns := customDeny
	if enableDeny {
		patterns = append(append([]string{}, defaultDenyPatterns...), platformDenyPatterns...)
		patterns = append(patterns, customDeny...)
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("bad deny pattern %q: %w", p, err)
		}
		g.patterns = append(g.patterns, re)
	}
	return g, nil
}

// Check reports why a command is refused, or nil to let it run.
func (g *CommandGuard) Check(command string) error {
	if g == nil {
		return nil
	}
	for _, re := range g.patterns {
		if re.MatchString(command) {
			return fmt.Errorf("command blocked by safety pattern %q — if this is intentional, "+
				"the user can adjust tools.custom_deny_patterns or run it themselves", re.String())
		}
	}
	return nil
}

type ExecTool struct {
	guard          *PathGuard
	timeout        time.Duration
	commands       *CommandGuard
	maxOutputBytes int
}

func NewExecTool(guard *PathGuard, timeout time.Duration, enableDeny bool, customDeny []string) (*ExecTool, error) {
	commands, err := NewCommandGuard(enableDeny, customDeny)
	if err != nil {
		return nil, err
	}
	t := &ExecTool{guard: guard, timeout: timeout, commands: commands, maxOutputBytes: 32 * 1024}
	if t.timeout <= 0 {
		t.timeout = 2 * time.Minute
	}
	return t, nil
}

// Commands exposes the compiled deny-list, so the background job engine runs
// its shell behind the same rail this one does.
func (t *ExecTool) Commands() *CommandGuard { return t.commands }

func (t *ExecTool) Name() string { return "exec" }
func (t *ExecTool) Description() string {
	return "Run a shell command (" + shellName + ") in the workspace. Returns combined output and exit code. Long or interactive commands will be killed at the timeout."
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
	if err := t.commands.Check(command); err != nil {
		return Errorf("%v", err)
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

	cmd := shellCommand(ctx, command)
	cmd.Dir = dir
	// Without these, a killed shell can leave grandchildren holding the output
	// pipe and Wait would block long past the timeout.
	cmd.WaitDelay = 2 * time.Second
	setProcessGroup(cmd)
	// Bounded while it runs rather than buffered and then cut. A command that
	// prints a gigabyte — a build in a loop, a `yes`, a log tailed by mistake —
	// used to be held in memory in full so that all but 32 KB of it could be
	// thrown away, and on the small machines this runs on that is the whole
	// box. What the model gets is the same either way.
	out := newBoundedOutput(t.maxOutputBytes)
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()

	text := out.String()

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

// boundedOutput captures a command's output as it is produced, keeping the
// first and last half of a budget and counting the bytes that fell between
// them. Both ends are worth keeping: a failing build says what it was doing
// at the top and why it stopped at the bottom.
type boundedOutput struct {
	mu      sync.Mutex
	half    int
	head    []byte
	tail    []byte
	omitted int
}

func newBoundedOutput(max int) *boundedOutput {
	if max < 2 {
		max = 2
	}
	return &boundedOutput{half: max / 2}
}

// Write implements io.Writer. It never grows past the budget: bytes past the
// head go into a tail window that drops its oldest as it fills.
func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := b.half - len(b.head); room > 0 {
		take := min(room, len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if len(p) > b.half {
		// One write larger than the whole window: only its tail can survive,
		// so it is never copied in. Without this the bound would hold only
		// because os/exec happens to copy in 32 KB pieces, which is not a
		// property of this type.
		b.omitted += len(b.tail) + len(p) - b.half
		b.tail = append(b.tail[:0], p[len(p)-b.half:]...)
		return n, nil
	}
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - b.half; over > 0 {
		b.tail = b.tail[over:]
		b.omitted += over
	}
	return n, nil
}

// String is the captured output, with a line naming what was dropped where
// it was dropped. A silent cut reads to the model as the whole answer.
func (b *boundedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.omitted == 0 {
		return string(b.head) + string(b.tail)
	}
	return fmt.Sprintf("%s\n... [%d bytes omitted; re-run with a narrower command or pipe through head/tail/grep] ...\n%s",
		b.head, b.omitted, b.tail)
}
