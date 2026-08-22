package tools

import (
	"runtime"
	"strings"
)

// These tests exercise the tools, not the shell they run commands in nor the
// platform's path syntax, so each fixture carries a Windows spelling beside
// its POSIX one.
func osPick(posix, windows string) string {
	if runtime.GOOS == "windows" {
		return windows
	}
	return posix
}

var (
	// Paths genuinely outside any workspace. On Windows a leading slash is not
	// absolute, so "/etc/passwd" resolves inside the workspace and would prove
	// the opposite of what the guard tests mean to assert.
	outsideDir     = osPick("/etc", `C:\Windows`)
	outsideFile    = osPick("/etc/hostname", `C:\Windows\win.ini`)
	outsideNewFile = osPick("/etc/factor-should-not-exist", `C:\Windows\factor-should-not-exist`)
	outsideAbs     = osPick("/etc/passwd", `C:\Windows\System32\drivers\etc\hosts`)

	cmdPrintCwd  = osPick("pwd", "cd")
	cmdSilent    = osPick("true", "rem")
	cmdStderrOne = osPick("echo to-stderr >&2; exit 1", "(echo to-stderr)1>&2& exit /b 1")
	cmdCatBig    = osPick("cat big.txt", "type big.txt")
	shellExe     = osPick("sh", "cmd")
)

// winArgv builds an argv that runs a one-liner through this platform's shell.
func winArgv(posix, windows string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/d", "/s", "/c", windows}
	}
	return []string{"sh", "-c", posix}
}

// isNotFoundMessage reports whether an error message reads as "that file is
// not there", in whichever words this platform's syscall layer uses.
func isNotFoundMessage(s string) bool {
	s = strings.ToLower(s)
	for _, m := range []string{"no such file", "file not found", "cannot find the file", "cannot find the path", "path not found"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
