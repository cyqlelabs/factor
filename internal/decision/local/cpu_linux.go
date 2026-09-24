package local

import (
	"os"
	"strings"
)

// hasAVX2 reports whether this CPU has the instructions Laya's int8 kernels
// need, from what the kernel says. A machine it cannot read is assumed
// capable: unknown is not the same as old.
var hasAVX2 = func() bool {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "flags") {
			return strings.Contains(" "+line+" ", " avx2 ")
		}
	}
	return true
}
