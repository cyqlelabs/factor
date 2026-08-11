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

	"github.com/cyqlelabs/factor/internal/app"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	_ "github.com/cyqlelabs/factor/internal/channel/telegram" // register connector
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/heartbeat"
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
	return pid, syscall.Kill(pid, 0) == nil
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
	defer os.Remove(pidPath())

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
	manager := channel.NewManager(a.Bus, channels)
	manager.Start(ctx)

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
	return nil
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
