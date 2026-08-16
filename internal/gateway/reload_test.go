package gateway

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
)

// fastSettle shrinks the restart pacing so a test does not wait out a real
// conversation's worth of grace.
func fastSettle(t *testing.T) {
	t.Helper()
	prevTimeout, prevPoll, prevGrace := settleTimeout, settlePoll, settleGrace
	settleTimeout, settlePoll, settleGrace = 2*time.Second, 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { settleTimeout, settlePoll, settleGrace = prevTimeout, prevPoll, prevGrace })
}

func TestSettleWaitsForTheAnswerToLand(t *testing.T) {
	fastSettle(t)

	ctx := context.Background()

	// Idle with an empty queue: nothing to wait for.
	start := time.Now()
	settle(ctx, func() bool { return true }, func() int { return 0 })
	if elapsed := time.Since(start); elapsed > settleTimeout {
		t.Errorf("settle took %v with nothing in flight", elapsed)
	}

	// A turn still running, then a reply still queued: settle holds until
	// both clear, so the restart cannot eat its own explanation.
	var turns, queued atomic.Int32
	turns.Store(3)
	queued.Store(2)
	settle(ctx,
		func() bool { return turns.Add(-1) <= 0 },
		func() int { return int(queued.Add(-1)) },
	)
	if turns.Load() > 0 || queued.Load() > 0 {
		t.Errorf("settle returned with %d turns and %d replies outstanding", turns.Load(), queued.Load())
	}
}

func TestSettleGivesUpOnAConversationThatNeverEnds(t *testing.T) {
	fastSettle(t)
	start := time.Now()
	settle(context.Background(), func() bool { return false }, func() int { return 1 })
	if elapsed := time.Since(start); elapsed < settleTimeout {
		t.Errorf("settle gave up after %v, before the %v timeout", elapsed, settleTimeout)
	}
}

func TestSettleStopsWaitingWhenTheDaemonIsToldToStop(t *testing.T) {
	fastSettle(t)
	settleTimeout = time.Minute // long enough that only the cancel can end it

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	settle(ctx, func() bool { return false }, func() int { return 1 })
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("settle kept waiting for %v after the daemon was told to stop", elapsed)
	}
}

func TestNotifyReloadTurnsSighupIntoARequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan string, 1)
	notifyReload(ctx, func(reason string) { got <- reason })

	if err := SignalRestart(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-got:
		if reason == "" {
			t.Error("restart requested without a reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not become a restart request")
	}
}

func TestRunRestartsIntoTheNewBinary(t *testing.T) {
	fastSettle(t)
	cfg, path := gatewayConfig(t)
	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))

	var relaunched atomic.Bool
	prev := relaunch
	relaunch = func() error { relaunched.Store(true); return nil }
	t.Cleanup(func() { relaunch = prev })

	// The chat the user last spoke in, as an earlier process recorded it. The
	// filename is the agent package's; a gateway that cannot read what the
	// loop wrote has no one to report back to after the restart.
	if err := os.WriteFile(filepath.Join(config.Home(), "last-channel.json"),
		[]byte(`{"channel":"telegram","chat_id":"42"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()

	if waitForHealth(t, healthURL, 20*time.Second) == nil {
		t.Fatal("gateway never served /health")
	}

	// What `factor upgrade` does to a daemon whose binary it just replaced.
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after a restart request")
	}
	if !relaunched.Load() {
		t.Error("the gateway shut down without execing the new binary")
	}
	// The exec happens after the daemon has let go of everything: a pid file
	// left behind would stop the new process from starting at all.
	if _, err := os.Stat(pidPath()); !os.IsNotExist(err) {
		t.Errorf("pid file survived the restart: %v", err)
	}
	// The one thing it does leave: who is waiting to hear that it came back.
	var sent []bus.OutboundMessage
	announceRestart(collector(&sent))
	if len(sent) != 1 || sent[0].Channel != "telegram" || sent[0].ChatID != "42" {
		t.Errorf("the restart left no notice for the last active chat: %+v", sent)
	}
}

func TestStopDuringTheRestartWaitDoesNotRelaunch(t *testing.T) {
	fastSettle(t)
	settleGrace = 3 * time.Second // hold the daemon in the wait long enough to stop it

	cfg, path := gatewayConfig(t)
	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))

	var relaunched atomic.Bool
	prev := relaunch
	relaunch = func() error { relaunched.Store(true); return nil }
	t.Cleanup(func() { relaunch = prev })

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()
	if waitForHealth(t, healthURL, 20*time.Second) == nil {
		t.Fatal("gateway never served /health")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the daemon is now inside the restart wait
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	// Being told to stop wins: the user asked for the process to end, not to
	// come back a moment later.
	if relaunched.Load() {
		t.Error("the daemon restarted after being told to stop")
	}
}

func TestShutdownDoesNotRelaunch(t *testing.T) {
	cfg, path := gatewayConfig(t)
	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))

	var relaunched atomic.Bool
	prev := relaunch
	relaunch = func() error { relaunched.Store(true); return nil }
	t.Cleanup(func() { relaunch = prev })

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()
	if waitForHealth(t, healthURL, 20*time.Second) == nil {
		t.Fatal("gateway never served /health")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	if relaunched.Load() {
		t.Error("a plain shutdown restarted the daemon")
	}
}
