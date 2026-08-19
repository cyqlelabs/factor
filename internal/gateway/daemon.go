package gateway

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// LogPath is where a backgrounded gateway writes what a foreground one would
// say on stderr.
func LogPath() string { return filepath.Join(config.Home(), "gateway.log") }

// Confirmation pacing, as variables so a test does not sit through a real
// startup window.
var (
	confirmTimeout = 10 * time.Second
	confirmPoll    = 50 * time.Millisecond
)

// A seam: a test must not re-exec the binary that is running it.
var daemonExecutable = os.Executable

// Daemonize starts `factor gateway` as a background process detached from
// this terminal and reports its pid once it is up. The pid comes from the pid
// file rather than the spawn: writePidFile records the writer's own pid, and
// it is the child, not this process, that serves.
// passthrough carries the flags that must survive the re-spawn. A detached
// gateway is a fresh process, so anything that configured this one — the
// proxy above all — has to be handed over on its command line, or the child
// runs without it and the log says nothing about the difference.
func Daemonize(configPath string, passthrough []string) (int, error) {
	if pid, alive := ReadPidFile(); alive {
		return 0, fmt.Errorf("gateway already running (pid %d)", pid)
	}
	exe, err := daemonExecutable()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	args := []string{"gateway"}
	if configPath != "" {
		args = append(args, "-c", configPath)
	}
	args = append(args, passthrough...)
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return awaitDaemon(exited)
}

// awaitDaemon watches for the child to claim the pid file, or to die trying —
// a taken port, a malformed config — in which case its last words are in the
// log and worth more than "it exited".
func awaitDaemon(exited <-chan error) (int, error) {
	deadline := time.Now().Add(confirmTimeout)
	for time.Now().Before(deadline) {
		if pid, alive := ReadPidFile(); alive {
			return pid, nil
		}
		select {
		case <-exited:
			return 0, fmt.Errorf("the gateway exited during startup: %s", lastLogLine())
		case <-time.After(confirmPoll):
		}
	}
	return 0, fmt.Errorf("the gateway did not come up within %s — see %s", confirmTimeout, LogPath())
}

// lastLogLine returns the final non-empty line of the gateway log, bounded so
// a long-lived log is not read whole for one line.
func lastLogLine() string {
	f, err := os.Open(LogPath())
	if err != nil {
		return "see " + LogPath()
	}
	defer f.Close()
	const tail = 4096
	var offset int64
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		offset = info.Size() - tail
	}
	data, err := io.ReadAll(io.NewSectionReader(f, offset, tail))
	if err != nil {
		return "see " + LogPath()
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return "see " + LogPath()
}
