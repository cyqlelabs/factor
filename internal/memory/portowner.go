package memory

// Finding the engine by the port it answers on.
//
// The pid file names the engine Factor spawned, and only that one. The engine
// actually serving is often something else: one started by hand after an
// upgrade said "it loads the next time the engine starts", or one adopted
// warm across a gateway reload. Both hold the memory port, which is the one
// fact that identifies the engine Factor talks to — so stopping it starts from
// the port when the pid file has nothing to say.

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// listenerLookup bounds the helper call on the platforms that need one.
const listenerLookup = 5 * time.Second

// ListenerPid reports the process listening on port on this machine, or
// false when nothing here can name one. Where several hold the socket — a
// server and the workers it forked — the lowest pid is the parent, and the one
// whose stop takes the rest with it.
func ListenerPid(port int) (int, bool) {
	if port <= 0 {
		return 0, false
	}
	var pids []int
	switch runtime.GOOS {
	case "linux":
		pids = procListeners(port)
	case "windows":
		pids = netstatListeners(port)
	default:
		pids = lsofListeners(port)
	}
	if len(pids) == 0 {
		return 0, false
	}
	lowest := pids[0]
	for _, pid := range pids[1:] {
		if pid < lowest {
			lowest = pid
		}
	}
	return lowest, true
}

// procListeners reads the kernel's socket tables: a listening socket on port
// is an inode, and the process holding it has that inode among its fds.
func procListeners(port int) []int {
	inodes := map[string]bool{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(table)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			// local_address st ... inode: state 0A is LISTEN.
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, hexPort, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			if p, err := strconv.ParseInt(hexPort, 16, 32); err == nil && int(p) == port {
				inodes[fields[9]] = true
			}
		}
		_ = f.Close()
	}
	if len(inodes) == 0 {
		return nil
	}
	var pids []int
	fdDirs, _ := filepath.Glob("/proc/[0-9]*/fd")
	for _, fdDir := range fdDirs {
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue // another user's process, which is not ours to stop either
		}
		for _, e := range entries {
			target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			if inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] {
				if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(fdDir))); err == nil {
					pids = append(pids, pid)
				}
				break
			}
		}
	}
	return pids
}

// lsofListeners asks lsof, which macOS ships, for the pids listening on port.
func lsofListeners(port int) []int {
	ctx, cancel := context.WithTimeout(context.Background(), listenerLookup)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// netstatListeners reads Windows' netstat, whose LISTENING rows end in the
// owning pid.
func netstatListeners(port int) []int {
	ctx, cancel := context.WithTimeout(context.Background(), listenerLookup)
	defer cancel()
	out, err := exec.CommandContext(ctx, "netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return nil
	}
	suffix := ":" + strconv.Itoa(port)
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[3] != "LISTENING" || !strings.HasSuffix(fields[1], suffix) {
			continue
		}
		if pid, err := strconv.Atoi(fields[4]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
