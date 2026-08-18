package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// TestMain lets a test re-exec this binary as the real CLI so main()'s
// command dispatch (including its os.Exit paths) can be asserted.
func TestMain(m *testing.M) {
	if args, ok := os.LookupEnv("FACTOR_TEST_MAIN_ARGS"); ok {
		os.Args = append([]string{"factor"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runCLI(t *testing.T, home, args string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"FACTOR_TEST_MAIN_ARGS="+args,
		"FACTOR_HOME="+home,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running the CLI: %v", err)
	}
	return string(out), code
}

func TestCLIVersion(t *testing.T) {
	out, code := runCLI(t, t.TempDir(), "version")
	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if !strings.HasPrefix(out, "factor ") || !strings.Contains(out, "built") {
		t.Errorf("version output = %q", out)
	}
}

func TestCLIInitAndStatus(t *testing.T) {
	home := t.TempDir()

	out, code := runCLI(t, home, "init")
	if code != 0 {
		t.Fatalf("init exit = %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Errorf("init did not write a config: %v", err)
	}

	out, code = runCLI(t, home, "status")
	if code != 0 {
		t.Fatalf("status exit = %d: %s", code, out)
	}
	if !strings.Contains(out, "gateway:") || !strings.Contains(out, "memory:") {
		t.Errorf("status output = %q", out)
	}
}

func TestCLIUnknownCommandExitsTwo(t *testing.T) {
	out, code := runCLI(t, t.TempDir(), "definitely-not-a-command")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("usage not printed: %q", out)
	}
}

func TestCLIReportsErrorsAndExitsOne(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, home, "status")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out, "factor:") {
		t.Errorf("error not reported on stderr: %q", out)
	}
}

// replConfig is a hermetic config whose provider fails fast.
func replConfig(t *testing.T, home string) string {
	t.Helper()
	cfg := config.Default()
	cfg.Memory.Mode = "off"
	cfg.Browser.Enabled = false
	cfg.Provider.APIBase = "http://127.0.0.1:1/v1"
	cfg.Provider.MaxRetries = 0
	cfg.Provider.RetryBackoffSecs = 1
	path := filepath.Join(home, "config.json")
	writeConfig(t, path, cfg)
	return path
}

// feedStdin replaces os.Stdin with a pipe and writes the given lines.
func feedStdin(t *testing.T, write func(w *os.File)) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = r
	go func() {
		write(w)
		_ = w.Close()
	}()
	return func() {
		os.Stdin = original
		_ = r.Close()
	}
}

func TestRunChatInteractiveSession(t *testing.T) {
	home := testHome(t)
	path := replConfig(t, home)

	restore := feedStdin(t, func(w *os.File) {
		fmt.Fprintln(w, "")      // blank input is ignored
		fmt.Fprintln(w, "/new")  // start a fresh session
		fmt.Fprintln(w, "/talk") // no voice channel: explained, not fatal
		fmt.Fprintln(w, "hello") // a real turn: the dead provider makes it fail
		time.Sleep(500 * time.Millisecond)
		fmt.Fprintln(w, "/quit")
	})
	defer restore()

	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = captureStdout(t, func() error { return runChat(path, "repl", "") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("interactive runChat never returned after /quit")
	}
	if err != nil {
		t.Fatalf("runChat = %v", err)
	}
	if !strings.Contains(out, "/quit to exit") {
		t.Errorf("banner missing: %q", out)
	}
	if !strings.Contains(out, "(started a fresh session)") {
		t.Errorf("/new not handled: %q", out)
	}
	if !strings.Contains(out, "PC voice is not running") {
		t.Errorf("/talk not handled: %q", out)
	}
	// the failed turn is surfaced through the bus-driven printer
	if !strings.Contains(out, "Something went wrong") {
		t.Errorf("turn failure not shown to the user: %q", out)
	}
}

func TestRunChatExitsOnStdinEOF(t *testing.T) {
	home := testHome(t)
	path := replConfig(t, home)

	restore := feedStdin(t, func(w *os.File) { fmt.Fprintln(w, "/exit") })
	defer restore()

	done := make(chan struct{})
	var err error
	go func() {
		_, err = captureStdout(t, func() error { return runChat(path, "eof", "") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runChat did not exit on /exit")
	}
	if err != nil {
		t.Errorf("runChat = %v", err)
	}
}
