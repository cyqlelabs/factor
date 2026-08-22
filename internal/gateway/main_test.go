package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for the factor binary Daemonize
// spawns, so the detach, the pid-file confirmation and the log capture run
// against a real child process on every platform. A shell script cannot play
// that part on Windows, and only the child itself knows the pid it must
// claim.
func TestMain(m *testing.M) {
	switch os.Getenv("FACTOR_TEST_GATEWAY_MODE") {
	case "serve":
		fmt.Printf("args: %s\n", strings.Join(os.Args[1:], " "))
		claimPidFile("factor.pid")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "die":
		fmt.Fprintln(os.Stderr, "factor: boom")
		os.Exit(3)
	case "silent":
		// Never claims the pid file: this is the startup-timeout path. The pid
		// it does record is only so the test can clean the process up.
		claimPidFile("child.pid")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func claimPidFile(name string) {
	home := os.Getenv("FACTOR_HOME")
	_ = os.MkdirAll(home, 0o755)
	_ = os.WriteFile(filepath.Join(home, name), []byte(strconv.Itoa(os.Getpid())), 0o600)
}
