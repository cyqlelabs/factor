//go:build linux

package local

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// availableMB reports how much memory the machine could hand a new process
// without swapping, and whether it could be read at all.
//
// MemAvailable rather than MemFree: the kernel's own estimate of what is
// reclaimable, which is the number that decides whether a 3 GB allocation
// thrashes. MemFree on a healthy box is mostly page cache and reads as far
// too little.
// totalMB is what the machine was built with, for the choice of engine:
// a number that does not move with what is open right now.
var totalMB = func() (int, bool) { return meminfo("MemTotal:") }

var availableMB = func() (int, bool) { return meminfo("MemAvailable:") }

// meminfo reads one /proc/meminfo line, in MB.
func meminfo(prefix string) (int, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, false
		}
		return kb / 1024, true
	}
	return 0, false
}
