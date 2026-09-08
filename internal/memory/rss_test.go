package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestRSSOfThisProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tasklist output is checked from a capture below")
	}
	rss, ok := processRSS(os.Getpid())
	if !ok || rss < 1<<20 {
		t.Errorf("rss of this process = %d, %v; want a real number of megabytes", rss, ok)
	}
	if _, ok := processRSS(1<<30 - 1); ok {
		t.Error("a pid nothing has was measured")
	}
}

// The macOS and Windows readers only run there, so their parsing is checked
// against captured output.
func TestRSSParsers(t *testing.T) {
	if n, ok := parsePsRSS(" 712344\n"); !ok || n != 712344<<10 {
		t.Errorf("ps = %d, %v", n, ok)
	}
	if _, ok := parsePsRSS("\n"); ok {
		t.Error("an empty ps answer was read as a size")
	}
	win := "\"smrti.exe\",\"6120\",\"Console\",\"1\",\"1,975,444 K\"\r\n"
	if n, ok := parseTasklistRSS(win); !ok || n != 1975444<<10 {
		t.Errorf("tasklist = %d, %v", n, ok)
	}
	if _, ok := parseTasklistRSS("INFO: No tasks are running which match the specified criteria.\r\n"); ok {
		t.Error("a missing task was read as a size")
	}
	saved := helperOutput
	helperOutput = func(string, ...string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { helperOutput = saved })
	if runtime.GOOS != "linux" {
		if _, ok := rssOf(os.Getpid()); ok {
			t.Error("a missing helper reported a size")
		}
	}
}

// The engine leaks; the supervisor restarts it for size once the graph is
// idle, and it comes back under the same supervisor with the same store.
// The engine here is the fake one this binary becomes, told it weighs three
// gigabytes.
func TestSupervisorRestartsTheEngineForSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	cfg := sidecarConfig(t, "serve")
	cfg.KeepAlive = false
	cfg.MaxRSSMB = 1024
	// The engine starts small and grows once it is up, so the test can
	// hold its first pid before the watchdog acts.
	var rss atomic.Int64
	rss.Store(100 << 20)
	saved, savedGap := processRSS, sizeRestartGap
	processRSS = func(int) (int64, bool) { return rss.Load(), true }
	sizeRestartGap = time.Hour
	t.Cleanup(func() { processRSS, sizeRestartGap = saved, savedGap })

	// Built by hand so the probe cadence is set before anything runs: the
	// supervisor's goroutines read it, and a test writing it afterwards is
	// a race the detector rightly reports.
	s := &Sidecar{
		client:        NewClient(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port), "", ""),
		cfg:           cfg,
		extract:       ExtractSettings{Mode: "local"},
		logDir:        t.TempDir(),
		probeInterval: 100 * time.Millisecond,
	}
	s.start(context.Background())
	var eng Engine = s
	defer func() { _ = eng.Close() }()
	if !waitHealthy(t, eng, 20*time.Second) {
		t.Fatal("engine never became healthy")
	}
	first, ok := readEnginePid()
	if !ok {
		t.Fatal("no engine pid recorded")
	}
	rss.Store(3 << 30) // the leak

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := readEnginePid(); ok && pid != first && eng.Healthy() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	second, ok := readEnginePid()
	if !ok || second == first {
		t.Fatalf("the engine was not restarted: pid %d → %d", first, second)
	}
	if s.SizeRestarts() != 1 {
		t.Errorf("size restarts = %d", s.SizeRestarts())
	}
	// Still over the ceiling, but inside the gap: left alone, since an engine
	// that refills in seconds is a bug to report rather than restart forever.
	time.Sleep(500 * time.Millisecond)
	if pid, _ := readEnginePid(); pid != second {
		t.Errorf("restarted again inside the gap: %d → %d", second, pid)
	}
}

// The check is off for an engine somebody else runs, when switched off, and
// while a turn is using the graph.
func TestSizeRestartIsHeldBack(t *testing.T) {
	saved := processRSS
	processRSS = func(int) (int64, bool) { return 3 << 30, true }
	t.Cleanup(func() { processRSS = saved })
	t.Setenv("FACTOR_HOME", t.TempDir())
	writeEnginePid(os.Getpid())

	cfg := sidecarConfig(t, "serve")
	cfg.MaxRSSMB = -1
	off := &Sidecar{client: NewClient("http://127.0.0.1:1", "", ""), cfg: cfg}
	if off.restartForSize(context.Background()) {
		t.Error("restarted with the ceiling switched off")
	}
	cfg.MaxRSSMB = 1024
	external := &Sidecar{client: NewClient("http://127.0.0.1:1", "", ""), cfg: cfg, external: true}
	if external.restartForSize(context.Background()) {
		t.Error("restarted an engine somebody else runs")
	}
	busy := &Sidecar{client: NewClient("http://127.0.0.1:1", "", ""), cfg: cfg}
	done := busy.client.activity()
	if busy.restartForSize(context.Background()) {
		t.Error("restarted under a request in flight")
	}
	done()
	if busy.SizeRestarts() != 0 {
		t.Error("counted a restart that did not happen")
	}
}
