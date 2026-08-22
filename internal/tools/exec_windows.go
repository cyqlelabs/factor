//go:build windows

package tools

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// platformDenyPatterns are the Windows spellings of the disasters
// defaultDenyPatterns blocks on POSIX. Same intent: destroying a filesystem,
// the boot record, or the shadow copies a user would restore from, plus the
// download-and-execute one-liner. Ordinary administration stays allowed.
var platformDenyPatterns = []string{
	`(?i)\bformat\s+[a-zA-Z]:`,
	`(?i)\b(del|erase)\b[^|]*\s[/-][sS]\b[^|]*\s[a-zA-Z]:\\?(\s|$)`,
	`(?i)\b(rd|rmdir)\b[^|]*\s[/-][sS]\b[^|]*\s[a-zA-Z]:\\?(\s|$)`,
	`(?i)\bdiskpart\b`,
	`(?i)\bvssadmin\b[^|]*\bdelete\b`,
	`(?i)\bbcdedit\b[^|]*\/(delete|set)\b`,
	`(?i)\bcipher\s+/w`,
	`(?i)\bfsutil\b[^|]*\b(deletejournal|setzerodata)\b`,
	`(?i)\bremove-item\b[^|]*-recurse\b[^|]*-force\b[^|]*\s[a-zA-Z]:\\?(\s|$|")`,
	`(?i)\breg(\.exe)?\s+delete\b[^|]*\bHKLM\b`,
	// The Windows curl|sh: fetch a script and run it in the same breath.
	`(?i)(downloadstring|downloadfile|invoke-webrequest|iwr)\b[^|]*\|[^|]*\b(iex|invoke-expression)\b`,
	`(?i)\b(iex|invoke-expression)\b[^|]*\b(downloadstring|invoke-webrequest|iwr)\b`,
}

// shellName is what the tool description calls the shell it runs commands in.
const shellName = "cmd /c"

// comspec is the command interpreter this machine actually uses. %COMSPEC% is
// how Windows names it; the literal is the fallback for a stripped environment.
func comspec() string {
	if v := os.Getenv("COMSPEC"); v != "" {
		return v
	}
	return `C:\Windows\System32\cmd.exe`
}

// shellCommand runs a command line through cmd.exe.
//
// The command line is handed over verbatim rather than assembled from Args:
// Go escapes an argument's inner quotes with backslashes, which cmd.exe does
// not understand, so `echo "hi"` would reach the shell as `echo \"hi\"`. With
// /s cmd strips the one outer pair of quotes and takes the rest literally,
// which is the only spelling that survives a command containing its own.
// CREATE_NO_WINDOW keeps a console from flashing up for every command a
// gateway runs in the background.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	shell := comspec()
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       syscall.EscapeArg(shell) + ` /d /s /c "` + command + `"`,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return cmd
}

// setProcessGroup kills the shell's whole process tree on cancel. Windows has
// no process group to signal, so the tree is walked by taskkill — without it a
// killed cmd.exe leaves grandchildren holding the output pipe, which is the
// same hang the unix build avoids by signalling the group.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
