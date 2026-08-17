package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
)

// ExtractSettings configure smrti's entity-extraction LLM calls.
type ExtractSettings struct {
	Mode  string // hybrid | llm | local
	URL   string // upstream base WITHOUT /v1 (smrti appends /v1/chat/completions)
	Model string
	Key   string
}

// DeriveExtract picks extraction settings: explicit config wins, then the
// first OpenAI-compatible provider candidate, else pure-local extraction.
func DeriveExtract(cfg config.MemoryConfig, providerCfg config.ProviderConfig) ExtractSettings {
	if cfg.ExtractMode == "local" {
		return ExtractSettings{Mode: "local"}
	}
	mode := cfg.ExtractMode
	if mode == "" {
		mode = "hybrid"
	}
	if cfg.ExtractURL != "" {
		return ExtractSettings{Mode: mode, URL: stripV1(cfg.ExtractURL), Model: cfg.ExtractModel}
	}
	base, key, model, ok := provider.OpenAICompatibleEndpoint(providerCfg)
	if !ok {
		return ExtractSettings{Mode: "local"}
	}
	if cfg.ExtractModel != "" {
		model = cfg.ExtractModel
	}
	return ExtractSettings{Mode: mode, URL: stripV1(base), Model: model, Key: key}
}

func stripV1(u string) string {
	return strings.TrimSuffix(strings.TrimSuffix(u, "/"), "/v1")
}

// NewEngine builds the memory engine for the configured mode.
// The returned engine is usable immediately; health flips asynchronously.
func NewEngine(ctx context.Context, cfg config.MemoryConfig, extract ExtractSettings, logDir string) (Engine, error) {
	switch cfg.Mode {
	case "off":
		return Noop{}, nil
	case "external":
		client := NewClient(cfg.BaseURL(), cfg.APIKey, extract.Key)
		s := &Sidecar{client: client, cfg: cfg, extract: extract, external: true}
		s.start(ctx)
		return s, nil
	case "sidecar", "":
		client := NewClient(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port), cfg.APIKey, extract.Key)
		s := &Sidecar{client: client, cfg: cfg, extract: extract, logDir: logDir}
		s.start(ctx)
		return s, nil
	default:
		return nil, fmt.Errorf("unknown memory mode %q (want sidecar, external, or off)", cfg.Mode)
	}
}

// Sidecar supervises a `smrti serve rest` child process (or, in external
// mode, just health-checks a server someone else runs). If a healthy server
// already listens on the configured port, it is adopted instead of spawning
// a duplicate.
type Sidecar struct {
	client   *Client
	cfg      config.MemoryConfig
	extract  ExtractSettings
	logDir   string
	external bool

	binaryMissing atomic.Bool
	installTried  atomic.Bool
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	probeInterval time.Duration // healthy re-probe cadence; zero means 30s
}

func (s *Sidecar) reprobeInterval() time.Duration {
	if s.probeInterval > 0 {
		return s.probeInterval
	}
	return 30 * time.Second
}

// resolveCommand locates the smrti executable, installing it when it is
// missing and memory.auto_install is on. The install runs at most once per
// process: a machine without a usable Python installer must not re-attempt a
// multi-minute install on every supervisor restart.
func (s *Sidecar) resolveCommand(ctx context.Context) (string, error) {
	if path, ok := FindSmrti(s.cfg.Command, config.Home()); ok {
		return path, nil
	}
	if !s.cfg.AutoInstall {
		return "", fmt.Errorf("%q not found in PATH and memory.auto_install is off (pip install smrti)", s.cfg.Command)
	}
	if s.installTried.Swap(true) {
		return "", fmt.Errorf("%q not found and the automatic install already failed this run", s.cfg.Command)
	}
	slog.Info("smrti not found; installing it automatically")
	path, method, err := Install(ctx, config.Home(), func(format string, args ...any) {
		slog.Info("smrti install: " + fmt.Sprintf(format, args...))
	})
	if err != nil {
		return "", err
	}
	slog.Info("smrti installed", "path", path, "method", method)
	return path, nil
}

func (s *Sidecar) start(parent context.Context) {
	// Synchronous first probe: a warm server (kept alive by a previous run,
	// or external) is recognized before the caller's first turn, so recall
	// works from message one instead of racing the async supervisor.
	probeCtx, probeCancel := context.WithTimeout(parent, 2*time.Second)
	_ = s.client.CheckHealth(probeCtx)
	probeCancel()

	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

func (s *Sidecar) run(ctx context.Context) {
	defer s.wg.Done()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		if s.client.CheckHealth(ctx) == nil {
			backoff = 5 * time.Second
			s.pollWhileHealthy(ctx)
			continue
		}
		if s.external {
			sleepCtx(ctx, 15*time.Second)
			continue
		}
		err := s.spawnAndWait(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("smrti sidecar exited; restarting", "error", err, "backoff", backoff)
		sleepCtx(ctx, backoff)
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

func (s *Sidecar) pollWhileHealthy(ctx context.Context) {
	for ctx.Err() == nil {
		sleepCtx(ctx, s.reprobeInterval())
		if ctx.Err() != nil || s.client.CheckHealth(ctx) != nil {
			return
		}
	}
}

func (s *Sidecar) buildEnv() []string {
	env := append(os.Environ(),
		"SMRTI_DB="+s.cfg.DBPath,
		"SMRTI_TENANT_ID="+s.cfg.Tenant,
		"SMRTI_SPACE="+s.cfg.Space,
		"SMRTI_PERSONALITY="+s.cfg.Personality,
		"SMRTI_REFLECT_INTERVAL="+strconv.Itoa(s.cfg.ReflectIntervalSecs),
		"SMRTI_EXTRACT_MODE="+s.extract.Mode,
	)
	if len(s.cfg.IgnorePatterns) > 0 {
		env = append(env, "SMRTI_IGNORE_PATTERNS="+strings.Join(s.cfg.IgnorePatterns, "\n"))
	}
	if s.extract.URL != "" {
		env = append(env, "SMRTI_EXTRACT_URL="+s.extract.URL)
	}
	if s.extract.Model != "" {
		env = append(env, "SMRTI_EXTRACT_MODEL="+s.extract.Model)
	}
	if s.cfg.APIKey != "" {
		env = append(env, "SMRTI_API_KEY="+s.cfg.APIKey)
	}
	return env
}

// spawnAndWait starts a detached smrti process and blocks until it exits or
// ctx is done. With keep_alive (the default) the process survives Factor's
// exit: one-shot commands and restarts adopt the warm engine instead of
// paying the cold start every time.
func (s *Sidecar) spawnAndWait(ctx context.Context) error {
	command, err := s.resolveCommand(ctx)
	if err != nil {
		s.binaryMissing.Store(true)
		return err
	}
	s.binaryMissing.Store(false)

	cmd := exec.Command(command, "serve", "rest",
		"--host", s.cfg.Host, "--port", strconv.Itoa(s.cfg.Port))
	cmd.Env = s.buildEnv()
	detachSidecar(cmd) // own session: survives Factor unless we signal it

	if s.logDir != "" {
		if err := os.MkdirAll(s.logDir, 0o755); err == nil {
			if f, err := os.OpenFile(filepath.Join(s.logDir, "smrti.log"),
				os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				defer f.Close()
				cmd.Stdout, cmd.Stderr = f, f
			}
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	slog.Info("smrti sidecar started", "pid", cmd.Process.Pid, "port", s.cfg.Port, "keep_alive", s.cfg.KeepAlive)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// Poll health for as long as the process lives. First start can be slow
	// (ONNX model download); warn at the startup timeout but keep waiting.
	// Polling must survive the first success: a request that times out while
	// the engine warms up flips the client unhealthy, and without a re-probe
	// that verdict would stick for the whole session even though the server
	// recovers moments later.
	pollCtx, stopPoll := context.WithCancel(context.Background())
	defer stopPoll()
	go func() {
		deadline := time.Now().Add(time.Duration(s.cfg.StartupTimeoutSecs) * time.Second)
		warned := false
		for pollCtx.Err() == nil {
			wasHealthy := s.client.Healthy()
			if s.client.CheckHealth(pollCtx) == nil {
				if !wasHealthy {
					slog.Info("smrti sidecar healthy")
				}
				sleepCtx(pollCtx, s.reprobeInterval())
				continue
			}
			if !warned && s.cfg.StartupTimeoutSecs > 0 && time.Now().After(deadline) {
				slog.Warn("smrti still not healthy; first run downloads models and can take a while",
					"timeout", s.cfg.StartupTimeoutSecs)
				warned = true
			}
			sleepCtx(pollCtx, 2*time.Second)
		}
	}()

	select {
	case err := <-waitCh:
		return err
	case <-ctx.Done():
		if s.cfg.KeepAlive {
			slog.Info("leaving smrti running for the next factor invocation", "pid", cmd.Process.Pid)
			go func() { <-waitCh }() // reap if it dies while we're still alive
			return ctx.Err()
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-waitCh:
			return err
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			return <-waitCh
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (s *Sidecar) Remember(ctx context.Context, req RememberRequest) (string, error) {
	return s.client.Remember(ctx, req)
}
func (s *Sidecar) Recall(ctx context.Context, query string, topK int, minConfidence float64, scope Scope) ([]Memory, error) {
	return s.client.Recall(ctx, query, topK, minConfidence, scope)
}
func (s *Sidecar) Forget(ctx context.Context, query, reason, space string) error {
	return s.client.Forget(ctx, query, reason, space)
}
func (s *Sidecar) SpaceSupport() (bool, string)                        { return s.client.SpaceSupport() }
func (s *Sidecar) Reflect(ctx context.Context) (map[string]any, error) { return s.client.Reflect(ctx) }
func (s *Sidecar) Status(ctx context.Context) (map[string]any, error)  { return s.client.Status(ctx) }
func (s *Sidecar) Enabled() bool                                       { return !s.binaryMissing.Load() }
func (s *Sidecar) Healthy() bool                                       { return s.client.Healthy() }

// Idle reports that the graph has been untouched for quiet — what the upgrade
// path waits for before restarting the engine underneath a live Factor.
func (s *Sidecar) Idle(quiet time.Duration) bool { return s.client.Idle(quiet) }

func (s *Sidecar) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}
