package phone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The voice shell is a Python process running Patter's pipeline: it terminates
// the carrier's webhooks and media stream, does speech-to-text and
// text-to-speech, handles turn-taking and barge-in, and calls back into the
// bridge for every reply. Factor supervises it the way it supervises smrti —
// spawn, health-poll, restart with backoff — and never treats its absence as
// fatal: a dead voice shell means no calls, not a dead gateway.

type supervisor struct {
	cfg        Config
	home       string
	scriptPath string
	shellCfg   func() (shellConfig, error)
	control    *controlClient
	external   bool // control_api_base set: something else runs the shell

	healthy       atomic.Bool
	down          atomic.Value // string: why the shell cannot run, "" when fine
	installTried  atomic.Bool
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	probeInterval time.Duration
}

func newSupervisor(cfg Config, home, token string, shellCfg func() (shellConfig, error)) *supervisor {
	base := cfg.ControlAPIBase
	external := base != ""
	if base == "" {
		base = fmt.Sprintf("http://127.0.0.1:%d", cfg.SidecarPort)
	}
	control := newControlClient(base)
	// Placing a call spends money, so the shell only takes that instruction
	// from whoever holds this boot's secret — even on loopback.
	control.token = token
	return &supervisor{
		cfg:        cfg,
		home:       home,
		scriptPath: filepath.Join(home, "voiceshell.py"),
		shellCfg:   shellCfg,
		control:    control,
		external:   external,
	}
}

func (s *supervisor) reprobeInterval() time.Duration {
	if s.probeInterval > 0 {
		return s.probeInterval
	}
	return 15 * time.Second
}

func (s *supervisor) Healthy() bool { return s.healthy.Load() }

// Down reports why calls cannot work right now ("" when nothing is wrong).
func (s *supervisor) Down() string {
	reason, _ := s.down.Load().(string)
	return reason
}

func (s *supervisor) setDown(format string, args ...any) {
	s.down.Store(fmt.Sprintf(format, args...))
}

func (s *supervisor) start(parent context.Context) {
	s.down.Store("")
	probeCtx, probeCancel := context.WithTimeout(parent, 2*time.Second)
	s.healthy.Store(s.control.health(probeCtx) == nil)
	probeCancel()

	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

func (s *supervisor) stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *supervisor) run(ctx context.Context) {
	defer s.wg.Done()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		if s.control.health(ctx) == nil {
			s.healthy.Store(true)
			s.down.Store("")
			backoff = 5 * time.Second
			s.pollWhileHealthy(ctx)
			continue
		}
		s.healthy.Store(false)
		if s.external {
			sleepCtx(ctx, s.reprobeInterval())
			continue
		}
		err := s.spawnAndWait(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("voice shell exited; restarting", "error", err, "backoff", backoff)
		sleepCtx(ctx, backoff)
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

func (s *supervisor) pollWhileHealthy(ctx context.Context) {
	for ctx.Err() == nil {
		sleepCtx(ctx, s.reprobeInterval())
		if ctx.Err() != nil {
			return
		}
		if s.control.health(ctx) != nil {
			s.healthy.Store(false)
			return
		}
	}
}

// installVoiceShell is Install behind a seam, so a test can exercise the
// install path — and a CI box without Python can be stopped from attempting
// one — without a multi-minute download.
var installVoiceShell = Install

// resolveCommand locates the Python that runs the voice shell, installing
// Patter into a private venv when it is missing. The install is attempted at
// most once per process: a machine without a usable Python must not re-run a
// multi-minute install on every supervisor restart.
func (s *supervisor) resolveCommand(ctx context.Context) (string, error) {
	if s.cfg.Command != "" {
		return resolveInterpreter(s.cfg.Command)
	}
	if path, ok := FindVoiceShellPython(s.home); ok {
		return path, nil
	}
	if !s.cfg.autoInstall() {
		return "", fmt.Errorf("the Patter voice shell is not installed and channels.phone.auto_install is off (%s)", InstallHint())
	}
	if s.installTried.Swap(true) {
		return "", fmt.Errorf("the Patter voice shell is not installed and the automatic install already failed this run")
	}
	slog.Info("voice shell dependencies missing; installing Patter automatically")
	path, err := installVoiceShell(ctx, s.home, func(format string, args ...any) {
		slog.Info("patter install: " + fmt.Sprintf(format, args...))
	})
	if err != nil {
		return "", err
	}
	slog.Info("Patter installed", "python", path)
	return path, nil
}

func (s *supervisor) spawnAndWait(ctx context.Context) error {
	command, err := s.resolveCommand(ctx)
	if err != nil {
		s.setDown("%v", err)
		return err
	}
	shell, err := s.shellCfg()
	if err != nil {
		s.setDown("%v", err)
		return err
	}
	blob, err := json.Marshal(shell)
	if err != nil {
		return err
	}
	if err := WriteScript(s.scriptPath); err != nil {
		s.setDown("%v", err)
		return err
	}
	s.down.Store("")

	cmd := exec.CommandContext(ctx, command, s.scriptPath)
	// Secrets ride in the environment, never in argv, where every process on
	// the machine could read them out of /proc.
	cmd.Env = append(os.Environ(), "FACTOR_VOICE_CONFIG="+string(blob))
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	logDir := filepath.Join(s.home, "logs")
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		if f, ferr := os.OpenFile(filepath.Join(logDir, "voiceshell.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
			defer f.Close()
			cmd.Stdout, cmd.Stderr = f, f
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	slog.Info("voice shell started",
		"pid", cmd.Process.Pid, "control_port", s.cfg.SidecarPort, "tier", s.cfg.TierLabel())

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	pollCtx, stopPoll := context.WithCancel(context.Background())
	defer stopPoll()
	go s.pollUntilHealthy(pollCtx)

	select {
	case err := <-waitCh:
		s.healthy.Store(false)
		return err
	case <-ctx.Done():
		// A live phone call must not outlive the agent: unlike smrti there is
		// no keep-alive here, the shell always goes down with us.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-waitCh:
			s.healthy.Store(false)
			return err
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			s.healthy.Store(false)
			return <-waitCh
		}
	}
}

// pollUntilHealthy tracks the child's health for as long as it lives. It keeps
// probing past the first success so a transient failure does not stick.
func (s *supervisor) pollUntilHealthy(ctx context.Context) {
	for ctx.Err() == nil {
		was := s.healthy.Load()
		if s.control.health(ctx) == nil {
			if !was {
				slog.Info("voice shell healthy", "port", s.cfg.SidecarPort)
			}
			s.healthy.Store(true)
			sleepCtx(ctx, s.reprobeInterval())
			continue
		}
		s.healthy.Store(false)
		sleepCtx(ctx, 2*time.Second)
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

// ---- control API client ----------------------------------------------------

// controlClient talks to the voice shell's own tiny API: is it alive, and
// please place this call.
type controlClient struct {
	base   string
	client *http.Client
	token  string
}

func newControlClient(base string) *controlClient {
	return &controlClient{base: base, client: &http.Client{Timeout: 20 * time.Second}}
}

func (c *controlClient) health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("voice shell health: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *controlClient) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

type placeCallRequest struct {
	To           string `json:"to"`
	Goal         string `json:"goal"`
	FirstMessage string `json:"first_message,omitempty"`
}

// placeCall asks the shell to dial. It returns as soon as the call is queued —
// the outcome comes back later, as a call-ended event.
func (c *controlClient) placeCall(ctx context.Context, req placeCallRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/call", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.authorize(httpReq)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("the voice shell is not reachable: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		CallID string `json:"call_id"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(data, &parsed)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := parsed.Error
		if detail == "" {
			detail = string(bytes.TrimSpace(data))
		}
		return "", fmt.Errorf("the voice shell refused the call: HTTP %d %s", resp.StatusCode, detail)
	}
	if parsed.CallID == "" {
		return "", fmt.Errorf("the voice shell accepted the call but returned no call id")
	}
	return parsed.CallID, nil
}
