//go:build unix

package gateway

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// notifyReload turns SIGHUP into a restart request, which is how `factor
// upgrade` in a terminal reaches the daemon whose binary it just replaced.
func notifyReload(ctx context.Context, request func(string)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				request("SIGHUP")
			}
		}
	}()
}

// SignalRestart asks the gateway running as pid to reload into the binary
// currently on disk. The signal is the shortest path and needs nothing to be
// listening, so the control endpoint is left to the platform that has no
// signal to send.
func SignalRestart(_ string, pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }
