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

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (s *Sidecar) start(parent context.Context) {
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
		sleepCtx(ctx, 30*time.Second)
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

func (s *Sidecar) spawnAndWait(ctx context.Context) error {
	command := s.cfg.Command
	if command == "" {
		command = "smrti"
	}
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("%q not found in PATH (pip install smrti): %w", command, err)
	}

	cmd := exec.CommandContext(ctx, command, "serve", "rest",
		"--host", s.cfg.Host, "--port", strconv.Itoa(s.cfg.Port))
	cmd.Env = s.buildEnv()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 8 * time.Second

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
	slog.Info("smrti sidecar started", "pid", cmd.Process.Pid, "port", s.cfg.Port)

	// Poll health while the process lives. First start can be slow (ONNX
	// model download); warn at the startup timeout but keep waiting.
	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	go func() {
		deadline := time.Now().Add(time.Duration(s.cfg.StartupTimeoutSecs) * time.Second)
		warned := false
		for pollCtx.Err() == nil {
			if s.client.CheckHealth(pollCtx) == nil {
				slog.Info("smrti sidecar healthy")
				return
			}
			if !warned && s.cfg.StartupTimeoutSecs > 0 && time.Now().After(deadline) {
				slog.Warn("smrti still not healthy; first run downloads models and can take a while",
					"timeout", s.cfg.StartupTimeoutSecs)
				warned = true
			}
			sleepCtx(pollCtx, 2*time.Second)
		}
	}()

	return cmd.Wait()
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
func (s *Sidecar) Recall(ctx context.Context, query string, topK int, minConfidence float64) ([]Memory, error) {
	return s.client.Recall(ctx, query, topK, minConfidence)
}
func (s *Sidecar) Forget(ctx context.Context, query, reason string) error {
	return s.client.Forget(ctx, query, reason)
}
func (s *Sidecar) Reflect(ctx context.Context) (map[string]any, error) { return s.client.Reflect(ctx) }
func (s *Sidecar) Status(ctx context.Context) (map[string]any, error)  { return s.client.Status(ctx) }
func (s *Sidecar) Healthy() bool                                       { return s.client.Healthy() }

func (s *Sidecar) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}
