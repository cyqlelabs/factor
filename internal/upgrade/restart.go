package upgrade

import (
	"fmt"
	"sync"
)

// Restarter is the seam between installing a release and reloading into it.
// Only a long-running Factor has anything to reload: the gateway fills this
// in with its graceful restart, while a one-shot CLI leaves it empty because
// the process is gone before the new binary could matter.
type Restarter struct {
	mu sync.Mutex
	fn func(reason string)
}

// Set registers what performs the restart.
func (r *Restarter) Set(fn func(reason string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fn = fn
}

// Request asks for a restart and reports whether anything is there to do it.
// The restart is asynchronous by necessity: the caller is inside the turn
// that has to be answered before the process may go.
func (r *Restarter) Request(reason string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	fn := r.fn
	r.mu.Unlock()
	if fn == nil {
		return false
	}
	fn(reason)
	return true
}

// Where this process was started from, resolved before anything can move it.
// Asking later does not work: Linux answers through /proc/self/exe, which
// follows the inode rather than the name, and an upgrade has by then renamed
// that inode aside and unlinked it — the answer would be a path with a
// " (deleted)" marker on it, pointing at a file that is gone.
var startPath, startPathErr = executablePath()

// selfPath is the file to exec when restarting: the same path this process
// was started from, which is exactly the one an upgrade replaces in place.
func selfPath() (string, error) {
	if startPathErr != nil {
		return "", fmt.Errorf("finding the running binary: %w", startPathErr)
	}
	return startPath, nil
}
