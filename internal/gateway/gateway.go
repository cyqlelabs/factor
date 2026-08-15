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
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	_ "github.com/cyqlelabs/factor/internal/channel/phone"    // register connector
	_ "github.com/cyqlelabs/factor/internal/channel/telegram" // register connector
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/heartbeat"
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

// Run starts the daemon and blocks until SIGINT/SIGTERM.
func Run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := writePidFile(); err != nil {
		return err
	}
	defer func() { _ = os.Remove(pidPath()) }()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()

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
		if runner, ok := ch.(channel.TurnRunner); ok {
			runner.BindTurnRunner(a.Loop.ProcessDirect)
		}
		if provider, ok := ch.(channel.Toolset); ok {
			a.Registry.Register(provider.Toolset()...)
		}
	}
	manager := channel.NewManager(a.Bus, channels)
	manager.Start(ctx)

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
		go hb.Run(ctx)
	}

	if cfg.Upgrade.Check {
		go upgrade.Watch(ctx, time.Duration(cfg.Upgrade.CheckIntervalHours)*time.Hour, version.Version,
			func(rel upgrade.Release) { announceRelease(rel, a.Loop.LastChannel, a.Bus.PublishOutbound) })
	}

	healthSrv, err := startHealthServer(cfg, a, manager)
	if err != nil {
		return err
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

	<-ctx.Done()
	slog.Info("shutting down")
	manager.Stop()
	a.Jobs.Wait()
	a.Loop.WaitBackground(30 * time.Second) // in-flight turns, memory stores, compaction
	return nil
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

func startHealthServer(cfg *config.Config, a *app.App, manager *channel.Manager) (*http.Server, error) {
	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":        version.Version,
			"uptime_seconds": int(time.Since(started).Seconds()),
			"memory_healthy": a.Memory.Healthy(),
			"channels":       manager.Names(),
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
