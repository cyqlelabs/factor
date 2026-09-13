// Package childproc ends a child process the way the platform allows.
//
// Factor supervises three long-running children — the memory engine, the voice
// shell and the speech server — and every one of them was stopped with
// Process.Signal(syscall.SIGTERM). On Windows that call is not a slow path but
// a refusal: Go's implementation answers anything except os.Kill with
// syscall.EWINDOWS, so nothing is sent, nothing dies, and the shutdown waits
// out its whole grace period before killing the process anyway. A reload with
// three sidecars up spent twenty seconds asking processes to stop in a
// language they do not speak.
//
// There is no graceful equivalent to reach for. A console control event only
// travels to processes sharing the sender's console, and a sidecar that must
// outlive the terminal Factor was typed into is given a console of its own for
// exactly that reason. So on Windows the stop is immediate and honest, and the
// grace period is skipped rather than waited out.
package childproc

import (
	"os"
	"time"
)

// StopAndWait asks p to finish and returns what Wait reported, killing it if
// it does not stop within grace. done carries the result of cmd.Wait.
//
// The grace period is only spent where the request means something: where it
// does not, waiting is time the user spends watching a shutdown that has
// already done everything it can.
func StopAndWait(p *os.Process, done <-chan error, grace time.Duration) error {
	if !Stop(p) {
		return <-done
	}
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		_ = p.Kill()
		return <-done
	}
}
