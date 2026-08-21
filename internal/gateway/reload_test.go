package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// The chat below is only an address while its connector is running, so
	// the daemon under test has to be the one that serves it.
	tg := fakeTelegram(t)
	cfg.Channels = map[string]json.RawMessage{
		"telegram": json.RawMessage(fmt.Sprintf(
			`{"token":"test-token","api_base":%q,"allow_from":["1"]}`, tg.URL)),
	}
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}
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

// The same chat, with its connector switched off. Nothing downstream may be
// told that address: the pump would drop whatever was sent to it, and the
// agent — voice_write, a cron result, this notice — would have reported a
// delivery that never happened.
func TestRestartLeavesNoNoticeForAChannelThisGatewayDoesNotRun(t *testing.T) {
	fastSettle(t)
	cfg, path := gatewayConfig(t)
	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))

	prev := relaunch
	relaunch = func() error { return nil }
	t.Cleanup(func() { relaunch = prev })

	if err := os.WriteFile(filepath.Join(config.Home(), "last-channel.json"),
		[]byte(`{"channel":"telegram","chat_id":"42"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()
	if waitForHealth(t, healthURL, 20*time.Second) == nil {
		t.Fatal("gateway never served /health")
	}
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

	var sent []bus.OutboundMessage
	announceRestart(collector(&sent))
	if len(sent) != 0 {
		t.Errorf("a notice was left for an unreachable chat: %+v", sent)
	}
}

// Editing config.json while the daemon runs is enough: the watcher notices,
// the gateway reloads itself through the same settle-and-exec path an upgrade
// takes, and the process that comes up tells the last active chat what was
// applied.
func TestRunReloadsWhenTheConfigFileChanges(t *testing.T) {
	fastSettle(t)
	prevPoll := configPoll
	configPoll = 20 * time.Millisecond
	t.Cleanup(func() { configPoll = prevPoll })

	cfg, path := gatewayConfig(t)
	// The notice goes to the last active chat, which is only an address while
	// its connector runs — so this daemon serves it.
	tg := fakeTelegram(t)
	cfg.Channels = map[string]json.RawMessage{
		"telegram": json.RawMessage(fmt.Sprintf(
			`{"token":"test-token","api_base":%q,"allow_from":["1"]}`, tg.URL)),
	}
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	healthURL := fmt.Sprintf("http://%s/health", net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port)))

	var relaunched atomic.Bool
	prev := relaunch
	relaunch = func() error { relaunched.Store(true); return nil }
	t.Cleanup(func() { relaunch = prev })

	if err := os.WriteFile(filepath.Join(config.Home(), "last-channel.json"),
		[]byte(`{"channel":"telegram","chat_id":"42"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- Run(path) }()
	if waitForHealth(t, healthURL, 20*time.Second) == nil {
		t.Fatal("gateway never served /health")
	}

	// The user edits the file by hand — or config_set writes it.
	cfg.Provider.Model = "a-newly-chosen-model"
	if err := os.WriteFile(path, mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the config change never reloaded the gateway")
	}
	if !relaunched.Load() {
		t.Error("the gateway shut down without execing itself")
	}

	// The note left behind names what changed, so the reload explains itself.
	var sent []bus.OutboundMessage
	announceRestart(collector(&sent))
	if len(sent) != 1 || !strings.Contains(sent[0].Content, "Config change applied (provider)") {
		t.Errorf("the reload notice = %+v", sent)
	}
}

// A config edit the reload would not survive — or would silently degrade
// under — is refused up front: the running configuration stays.
func TestPreflightRefusesAConfigTheReloadWouldNotSurvive(t *testing.T) {
	current := config.Default()
	current.Gateway.Host, current.Gateway.Port = "127.0.0.1", freePort(t)

	// A provider nothing can build.
	bad := config.Default()
	bad.Provider.Type = "no-such-provider"
	if err := preflight(current, bad); err == nil {
		t.Error("an unbuildable provider chain passed preflight")
	}

	// A channel section its connector refuses; Build would skip it with a
	// log line, which under a live reload silently drops the channel.
	bad = config.Default()
	bad.Channels = map[string]json.RawMessage{
		"voice": json.RawMessage(`{"activation":"sometimes"}`),
	}
	if err := preflight(current, bad); err == nil || !strings.Contains(err.Error(), "channels.voice") {
		t.Errorf("a broken channel section passed preflight: %v", err)
	}

	// A health address something else already holds.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	bad = config.Default()
	bad.Gateway.Host = "127.0.0.1"
	bad.Gateway.Port = taken.Addr().(*net.TCPAddr).Port
	if err := preflight(current, bad); err == nil {
		t.Error("an occupied gateway address passed preflight")
	}

	// The address it already serves on is not re-probed: the old process
	// still holds it, and it is released before the exec.
	if err := preflight(current, current); err != nil {
		t.Errorf("an unchanged config failed preflight: %v", err)
	}
}
