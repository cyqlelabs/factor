package jobs

import "runtime"

// The engine is what these tests exercise, not the shell it runs commands in,
// so each snippet carries a cmd.exe spelling beside its POSIX one.
var (
	payloadOK      = shellPick("true", "rem")
	payloadSleep   = shellPick("sleep 30", "ping -n 31 127.0.0.1 >nul")
	payloadFailErr = shellPick("echo oops >&2; exit 2", "echo oops 1>&2& exit /b 2")
	payloadFlood   = shellPick("yes 0123456789 | head -c 100000", "for /l %i in (1,1,20000) do @echo 0123456789")
)

func shellPick(posix, windows string) string {
	if runtime.GOOS == "windows" {
		return windows
	}
	return posix
}
