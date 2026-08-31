// Package gateway runs Factor as a daemon: channels, cron, heartbeat,
// background jobs, and a local health endpoint.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/app"
	"github.com/cyqlelabs/factor/internal/bands"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	_ "github.com/cyqlelabs/factor/internal/channel/phone"    // register connector
	_ "github.com/cyqlelabs/factor/internal/channel/telegram" // register connector
	_ "github.com/cyqlelabs/factor/internal/channel/voice"    // register connector
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/heartbeat"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/upgrade"
	"github.com/cyqlelabs/factor/internal/version"
)

func pidPath() string { return filepath.Join(config.Home(), "factor.pid") }

// ReadPidFile reports the daemon pid and whether that process is alive.
func ReadPidFile() (int, bool) {
	data, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, pidAlive(pid)
}

func writePidFile() error {
	if pid, alive := ReadPidFile(); alive {
		return fmt.Errorf("gateway already running (pid %d)", pid)
	}
	if err := os.MkdirAll(config.Home(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// A seam: a test must not exec away the process running it.
var relaunch = upgrade.Relaunch

// stopRequests carries a stop asked from inside this process — the tray's
// quit item — into serve's select, where it ends the daemon the way SIGTERM
// does. Buffered, so the click that delivers it never blocks on the loop.
var stopRequests = make(chan struct{}, 1)

// RequestStop asks the gateway running in this process to shut down cleanly.
func RequestStop() {
	select {
	case stopRequests <- struct{}{}:
	default: // one request is all it takes
	}
}

// Run starts the daemon and blocks until SIGINT/SIGTERM — or until an
// upgrade asks it to reload, in which case it shuts down cleanly and then
// execs the binary now on disk. The exec happens out here, after serve has
// closed the sidecars and dropped the pid file, so the new process inherits
// nothing the old one was still holding.
func Run(configPath string) error {
	reloading, err := serve(configPath)
	if err != nil || !reloading {
		return err
	}
	slog.Info("restarting into the newly installed factor")
	if err := relaunch(); err != nil {
		return fmt.Errorf("restarting into the new factor: %w", err)
	}
	return nil
}

func serve(configPath string) (bool, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return false, err
	}
	if err := writePidFile(); err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(pidPath()) }()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a, err := app.New(ctx, cfg)
	if err != nil {
		return false, err
	}
	defer a.Close()

	// Both ways of asking for a reload land here: the upgrade tool mid
	// conversation, and a SIGHUP from `factor upgrade` in a terminal.
	restart := make(chan restartRequest, 1)
	request := func(reason string, target upgrade.Target) {
		select {
		case restart <- restartRequest{reason: reason, target: target}:
		default: // one request is all it takes
		}
	}
	a.Restart.Set(request)
	// A SIGHUP asks from a terminal, with no conversation behind it.
	notifyReload(ctx, func(reason string) { request(reason, upgrade.Target{}) })
	// The config file is watched for as long as the daemon runs: a change —
	// hand-edited, or written by config_set from any session — reloads the
	// gateway through the same settle-and-exec path an upgrade takes. That is
	// what makes every parameter apply within seconds of saving, without
	// dropping the turn in flight or restarting the sidecars, whose live
	// engines the new process finds on their ports and adopts. An edit the
	// reload would not survive is refused up front, and the refusal reaches
	// the user rather than only the log: a live reload must not turn a typo
	// into an outage.
	go config.Watch(ctx, cfg, configPoll, func(next *config.Config, sections []string) {
		if err := preflight(cfg, next); err != nil {
			slog.Warn("config changed on disk but cannot be applied; keeping the running one", "error", err)
			if ch, chat, ok := a.Loop.LastChannel(); ok {
				a.Bus.PublishOutbound(bus.OutboundMessage{Channel: ch, ChatID: chat, Content: fmt.Sprintf(
					"The config change was not applied — %v. The previous configuration is still running.", err)})
			}
			return
		}
		slog.Info("config changed on disk; reloading to apply it", "sections", sections)
		request(configReloadPrefix+strings.Join(sections, ", "), upgrade.Target{})
	})

	channels := channel.Build(cfg.Channels, a.Bus)
	if len(channels) == 0 {
		slog.Warn("no channels configured; the gateway is only reachable via cron/heartbeat",
			"known_connectors", channel.Registered())
	}
	// Optional connector capabilities, both daemon-only: a connector whose
	// conversations are synchronous (the phone) runs turns itself, and one
	// that brings its own tools contributes them here — so a CLI session never
	// sees a tool that has no connector behind it.
	for _, ch := range channels {
		channel.BindTurns(ch, a.Loop.ProcessDirectNotice, a.Loop.ProcessDirectSteering)
		if guarded, ok := ch.(channel.Guarded); ok {
			guarded.BindPathGuard(a.Guard)
		}
		if addresser, ok := ch.(channel.Addresser); ok {
			addresser.BindLastExternal(a.Loop.LastChannel)
		}
		if provider, ok := ch.(channel.Toolset); ok {
			a.Registry.Register(provider.Toolset()...)
		}
	}
	began := time.Now()
	manager := channel.NewManager(a.Bus, channels)
	// Everything Factor says on its own initiative follows the user to the
	// chat they last used — which is only an address while that connector is
	// running. Without this, a gateway started with Telegram switched off
	// keeps writing to it: the message is dropped by the pump, and the agent,
	// told the tool succeeded, says it sent something it did not.
	a.Loop.SetReachable(manager.Serves)
	// ask_user follows the same rule one step further: a question raised by a
	// turn from a bus-riding chat is asked in that chat, and only a turn with
	// no such chat behind it falls back to the desktop dialog.
	a.Loop.SetConversational(manager.Conversational)
	manager.Start(ctx)

	// The tray's overview reads from here for as long as serve runs; the
	// deferred nil runs before a.Close, so a late ask never reaches a closed
	// engine.
	setStatusSource(func() []string {
		return statusLines(version.Version, time.Since(began),
			a.Memory.Enabled(), a.Memory.Healthy(), manager.Names(), a.Cost.OverviewLine())
	})
	defer setStatusSource(nil)

	// If the last process went down on purpose, say so: an upgrade asked over
	// Telegram is only finished once the new binary answers in that chat.
	announceRestart(a.Bus.PublishOutbound)

	// Let the chat show that a turn is running: every phase but the last is
	// still work in progress, and what the agent says on its way to an answer
	// is sent as it happens rather than held until the turn ends.
	a.Loop.OnActivity(func(act agent.Activity) {
		manager.SetTyping(act.SessionKey, act.Phase != agent.PhaseDone)
		if act.Phase == agent.PhaseNotice {
			manager.Interim(act.SessionKey, act.Detail)
		}
	})

	go a.Cron.Run(ctx)

	if cfg.Heartbeat.Enabled {
		hb := heartbeat.NewService(
			cfg.Agent.Workspace,
			time.Duration(cfg.Heartbeat.IntervalMinutes)*time.Minute,
			a.Loop.ProcessEphemeral,
			func(content string) bool {
				ch, chat, ok := a.Loop.LastChannel()
				if !ok {
					return false
				}
				return a.Bus.PublishOutbound(bus.OutboundMessage{Channel: ch, ChatID: chat, Content: content})
			},
		)
		// Detection is arithmetic over the traces this process already
		// writes, so the heartbeat now has something to notice besides what
		// the user wrote down — and still spends nothing when nothing moved.
		if dir := a.Traces(); dir != "" {
			hb = hb.WithBands(bands.New(dir).Check)
		}
		go hb.Run(ctx)
	}

	if cfg.Upgrade.Check {
		every := time.Duration(cfg.Upgrade.CheckIntervalHours) * time.Hour
		go upgrade.Watch(ctx, every, version.Version,
			func(rel upgrade.Release) { announceRelease(rel, a.Loop.LastChannel, a.Bus.PublishOutbound) })
		if a.SmrtiUpgrade != nil {
			go a.SmrtiUpgrade.Watch(ctx, every,
				func(rel upgrade.SmrtiRelease) { announceEngine(rel, a.Loop.LastChannel, a.Bus.PublishOutbound) })
		}
	}

	healthSrv, err := startHealthServer(cfg, a, manager)
	if err != nil {
		return false, err
	}
	defer func() {
		shutdownCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = healthSrv.Shutdown(shutdownCtx)
	}()

	go a.Loop.Run(ctx)

	slog.Info("factor gateway running",
		"version", version.Version,
		"channels", manager.Names(),
		"health", fmt.Sprintf("http://%s:%d/health", cfg.Gateway.Host, cfg.Gateway.Port))

	reloading := false
	select {
	case <-ctx.Done():
	case <-stopRequests:
		cancel()
	case req := <-restart:
		slog.Info("restart requested", "reason", req.reason)
		settle(ctx, a.Loop.Idle, a.Bus.PendingOutbound)
		// A stop that lands while the answer is still going out wins: the
		// user asked for this process to end, not to come back.
		reloading = ctx.Err() == nil
		if reloading {
			noteRestart(req, a.Loop.LastChannel, manager.Serves)
		}
		cancel()
	}
	slog.Info("shutting down")
	manager.Stop()
	a.Jobs.Wait()
	a.Loop.WaitBackground(30 * time.Second) // in-flight turns, memory stores, compaction
	return reloading, nil
}

// Restart pacing, as variables so a test does not sit through a real
// conversation.
var (
	settleTimeout = 60 * time.Second
	settlePoll    = 200 * time.Millisecond
	settleGrace   = 2 * time.Second

	// configPoll is how often the config file is checked for changes — the
	// ceiling on how long a saved edit waits to take effect.
	configPoll = 3 * time.Second
)

// preflight rejects a config edit the reload would not survive, or would
// silently degrade under: a provider chain that cannot be built, a channel
// section its connector refuses, a health address nothing can listen on.
// Cheap static checks only — what they cannot see (a wrong credential, a bad
// memory endpoint) fails exactly as it would after a hand restart.
func preflight(current, next *config.Config) error {
	if _, err := provider.BuildChain(next.Provider); err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	if err := channel.Validate(next.Channels); err != nil {
		return err
	}
	nextAddr := net.JoinHostPort(next.Gateway.Host, strconv.Itoa(next.Gateway.Port))
	if nextAddr != net.JoinHostPort(current.Gateway.Host, strconv.Itoa(current.Gateway.Port)) {
		ln, err := net.Listen("tcp", nextAddr)
		if err != nil {
			return fmt.Errorf("gateway address %s: %w", nextAddr, err)
		}
		_ = ln.Close()
	}
	return nil
}

// settle waits for the conversation that asked for the restart to be
// answered: every turn ends, its reply drains off the outbound queue, and
// the connector gets a moment to hand that reply to the network. Reloading
// any earlier is a restart that eats its own explanation.
func settle(ctx context.Context, idle func() bool, pending func() int) {
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) && (!idle() || pending() > 0) {
		if !pause(ctx, settlePoll) {
			return
		}
	}
	pause(ctx, settleGrace) // the last send is still on the wire
}

// pause waits out d, or gives up the moment ctx ends.
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// announceRelease tells the user a newer Factor exists, wherever they last
// spoke to this one. It is a log line and nothing more when no chat has
// happened yet: there is no one to tell, and the news keeps.
func announceRelease(rel upgrade.Release, last func() (string, string, bool), publish func(bus.OutboundMessage) bool) {
	slog.Info("a newer factor is available", "release", rel.Version, "running", version.Version)
	ch, chat, ok := last()
	if !ok {
		return
	}
	publish(bus.OutboundMessage{Channel: ch, ChatID: chat, Content: fmt.Sprintf(
		"factor %s is out — this one is %s. Ask me to upgrade, or run `factor upgrade`.\n%s",
		rel.Version, version.Version, rel.Notes)})
}

// announceEngine does the same for a newer smrti image. Installing it never
// takes Factor down — the engine is swapped in place while the graph is idle —
// but it is still the user's call, not the daemon's.
func announceEngine(rel upgrade.SmrtiRelease, last func() (string, string, bool), publish func(bus.OutboundMessage) bool) {
	slog.Info("a newer smrti is available", "published", rel.Version, "running", rel.Running, "mode", rel.Mode)
	ch, chat, ok := last()
	if !ok {
		return
	}
	publish(bus.OutboundMessage{Channel: ch, ChatID: chat, Content: fmt.Sprintf(
		"smrti %s is out — the memory engine here runs %s. Ask me to upgrade it, or run `factor upgrade`.",
		rel.Version, rel.RunningVersion())})
}

func startHealthServer(cfg *config.Config, a *app.App, manager *channel.Manager) (*http.Server, error) {
	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":        version.Version,
			"uptime_seconds": int(time.Since(started).Seconds()),
			"memory_healthy": a.Memory.Healthy(),
			// What a `factor upgrade` in a terminal waits for before it swaps
			// the engine's container: only this process knows what it has in
			// flight against the graph.
			"memory_idle": memory.IdleFunc(a.Memory, memory.UpgradeQuiet)(),
			"channels":    manager.Names(),
		})
	})
	addr := net.JoinHostPort(cfg.Gateway.Host, strconv.Itoa(cfg.Gateway.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("health listener on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server failed", "error", err)
		}
	}()
	return srv, nil
}
