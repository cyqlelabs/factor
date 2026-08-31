package memory

// Where the engine's own process is written down, and how it is stopped.
//
// keep_alive means the engine outlives the Factor that spawned it, so the
// process serving memory is not something a later run can find among its own
// children — and stopping it is exactly what upgrading a package install has to
// do, since a running interpreter keeps the code it imported. The file is
// written on every spawn and read through a liveness check, so a stale one left
// by an engine that died says nothing.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// engineStopWait is how long a stopping engine gets to exit on its own. smrti
// runs reflection passes on its own schedule and SIGKILL mid-pass throws that
// epoch away, so the polite signal is given room before the blunt one.
var engineStopWait = 30 * time.Second

func enginePidPath() string { return filepath.Join(config.Home(), "smrti.pid") }

// writeEnginePid records the engine this process just spawned. Best effort: a
// pid that cannot be written costs a later upgrade its restart, which is worth
// a log line rather than a failed spawn.
func writeEnginePid(pid int) {
	_ = os.WriteFile(enginePidPath(), []byte(strconv.Itoa(pid)), 0o600)
}

// EnginePid reports the engine Factor spawned and whether it is still running.
// Whoever stopped one is how the caller tells a supervisor that put a
// replacement in its place from a machine where nothing will.
func EnginePid() (int, bool) { return readEnginePid() }

// readEnginePid reports the recorded engine pid and whether that process is
// still alive.
func readEnginePid() (int, bool) {
	data, err := os.ReadFile(enginePidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, pidAlive(pid)
}

// clearEnginePid forgets an engine that has exited, but only while the file
// still names it: a supervisor that has already spawned the replacement wrote
// its pid there, and removing that would leave the new engine unstoppable.
func clearEnginePid(pid int) {
	if recorded, _ := readEnginePid(); recorded == pid {
		_ = os.Remove(enginePidPath())
	}
}

// StopEngine stops the smrti Factor spawned and reports which process it was.
// A zero pid is an answer rather than a failure: the engine may be one somebody
// runs by hand, or nothing may be running at all — in both cases the newly
// installed code is what the next start will load.
func StopEngine(ctx context.Context) (int, error) {
	pid, ok := readEnginePid()
	if !ok {
		return 0, nil
	}
	if err := terminateProcess(pid); err != nil {
		return 0, fmt.Errorf("stopping the memory engine (pid %d): %w", pid, err)
	}
	deadline := time.Now().Add(engineStopWait)
	for pidAlive(pid) {
		if ctx.Err() != nil {
			return pid, ctx.Err()
		}
		if time.Now().After(deadline) {
			killProcess(pid) // it had its chance to finish the epoch
			break
		}
		sleepCtx(ctx, 200*time.Millisecond)
	}
	clearEnginePid(pid) // a supervisor may already have written its replacement down
	return pid, nil
}
