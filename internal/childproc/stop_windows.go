//go:build windows

package childproc

import "os"

// Stop ends the process. Windows has no signal that reaches a child holding
// its own console, so this is the blunt one and there is nothing to wait for:
// reporting otherwise is what made every shutdown pay a grace period for a
// request that was never sent.
func Stop(p *os.Process) bool {
	if p == nil {
		return false
	}
	_ = p.Kill()
	return false
}
