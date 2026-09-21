//go:build unix

package local

import "syscall"

// modelNice is where the decision model sits against everything else on the
// machine. It is deliberately most of the way down: the model is the one
// process here whose work can always wait a second, and the machines that
// need this most are the ones with two slow cores, where the warm-up is the
// difference between a box that answers and a box that has to be power-cycled.
const modelNice = 15

// lowerPriority puts the model below the rest of the machine. A failure is
// nothing to report: the model runs either way, and a kernel that refuses is
// saying this process may not renice, not that anything is broken.
func lowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, modelNice)
}
