package memory

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// processRSS reports a process's resident set in bytes. Linux is read from
// the kernel; macOS and the BSDs through ps, which they all ship; Windows
// through tasklist. A var so the size watchdog can be tested against an
// engine that is not really that large.
var processRSS = rssOf

func rssOf(pid int) (int64, bool) {
	switch runtime.GOOS {
	case "linux":
		return procRSS(pid)
	case "windows":
		out, err := helperOutput("tasklist", "/NH", "/FO", "CSV", "/FI", "PID eq "+strconv.Itoa(pid))
		if err != nil {
			return 0, false
		}
		return parseTasklistRSS(out)
	default:
		out, err := helperOutput("ps", "-o", "rss=", "-p", strconv.Itoa(pid))
		if err != nil {
			return 0, false
		}
		return parsePsRSS(out)
	}
}

// procRSS reads VmRSS from /proc, which is in kilobytes.
func procRSS(pid int) (int64, bool) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if kb, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			fields := strings.Fields(kb)
			if len(fields) == 0 {
				return 0, false
			}
			n, err := strconv.ParseInt(fields[0], 10, 64)
			return n << 10, err == nil
		}
	}
	return 0, false
}

// parsePsRSS reads `ps -o rss=`, one number in kilobytes.
func parsePsRSS(out string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n << 10, err == nil
}

// parseTasklistRSS reads tasklist's CSV, whose last field is the working set
// as "123,456 K".
func parseTasklistRSS(out string) (int64, bool) {
	line := strings.TrimSpace(out)
	if line == "" || !strings.HasPrefix(line, "\"") {
		return 0, false // "INFO: No tasks are running" and the like
	}
	fields := strings.Split(line, "\",\"")
	last := strings.Trim(fields[len(fields)-1], "\"\r\n ")
	last = strings.TrimSuffix(strings.TrimSpace(last), " K")
	last = strings.ReplaceAll(strings.ReplaceAll(last, ",", ""), ".", "")
	n, err := strconv.ParseInt(last, 10, 64)
	return n << 10, err == nil
}
