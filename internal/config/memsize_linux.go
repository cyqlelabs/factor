//go:build linux

package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// totalMemoryMB is what this machine has, or zero when it cannot be read.
var totalMemoryMB = func() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
