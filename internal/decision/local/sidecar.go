package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cyqlelabs/factor/internal/childproc"
	"github.com/cyqlelabs/factor/internal/decision"
)

// The server is supervised the way the memory engine and the speech server
// are — spawn, health-poll, restart with a backoff — and its absence is never
// fatal: a decision that cannot be had is a decision the caller falls back
// from, which is the contract every caller of this package already honours.

const (
	// probeEvery is how often a healthy server is re-checked.
	probeEvery = 15 * time.Second
	// stopGrace is how long the child gets to exit on its own.
	stopGrace = 5 * time.Second
	// maxBackoff is how far apart the restarts get. It is minutes rather
	// than the usual seconds because of what a permanent failure costs here:
	// a machine that cannot reach the weights fails on every attempt, and
	// each attempt spawns a Python process that imports the runtime before it
	// can find that out. Observed against a blocked download, a one-minute
	// ceiling meant thirty-six of those in half an hour. Backing off to a
	// quarter of an hour keeps the feature self-healing — the network may
	// come back — without spending the machine on finding out.
	maxBackoff = 15 * time.Minute
	// loadAttempts is how many times a model that starts but never finishes
	// loading is given another go before the supervisor stops asking. Three
	// is already an hour of a slow machine's whole CPU; the fourth is not
	// going to be the one that works.
	loadAttempts = 3
	// minAvailableMB is the memory the model needs to be handed before it is
	// worth starting. The int8 graph is memory-mapped rather than built in
	// float32: measured serving the real artifact, resident size settles at
	// 559 MB against the 2942 MB the torch runtime peaked at, which is the
	// whole reason that runtime is gone.
	//
	// The floor is set well above the measurement because what it protects
	// against is a machine with nothing to spare: on 3535 MB of RAM the old
	// load took a shell command from 25 ms to 1061 ms and the box had to be
	// power-cycled. A decision that falls back costs nothing by comparison.
	minAvailableMB = 1024
	// charsPerToken converts the server's token budgets into the character
	// budgets a caller can actually measure. Deliberately conservative:
	// three is about right for English prose and pessimistic for the
	// languages that tokenize worse, and the cost of guessing low is a
	// shorter state rather than a silent truncation inside the model.
	charsPerToken = 3
	// tokensPerOption is what one offered candidate costs in the question
	// head: an index, a label, and the punctuation between them. A browser
	// control's label runs to a dozen tokens, and the head budget is shared
	// by every option in the question.
	tokensPerOption = 12
	// maxOfferedCandidates caps what any one question offers, under what the
	// token budget alone would allow. Laya's own benchmarks put it behind
	// the hosted model once a question carries more than about twenty
	// options (Banking77, 77 labels: 0.425 against 0.870), so this is an
	// accuracy bound as much as a size one.
	maxOfferedCandidates = 16
)

// Backend is the managed local decision model: a supervised child that
// answers the same /v1/systemone contract the hosted model does, and the
// client that talks to it.
type Backend struct {
	cfg    Config
	home   string
	client *client

	healthy atomic.Bool
	down    atomic.Value // string
	limits  atomic.Value // decision.Limits

	installTried atomic.Bool
	// installOK gates the install itself. Building the virtualenv pulls the
	// runtime wheels and then the model artifact behind it, a few hundred
	// megabytes together, and a one-shot `factor "what time is it"` that
	// starts that download and is then killed halfway helps nobody. So it is
	// permitted by the gateway, which is long enough lived to finish it, or
	// by the first decision actually asked for on this machine — the same
	// rule the browser engine follows.
	installOK atomic.Bool
	// installMu is what keeps the two callers that may install — the
	// gateway's Provision and the supervisor's own resolveCommand — from
	// doing it at the same time. They ran together on a fresh box and the
	// second one died on the virtualenv the first had just created; had they
	// got past that, two pips would have been filling one directory.
	// Whichever waits finds the install finished and takes it.
	installMu sync.Mutex
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// neverReady counts the runs that started and never finished loading. It
	// belongs to the supervisor goroutine.
	neverReady int

	// probeInterval lets a test shrink the poll.
	probeInterval time.Duration
	// httpClient is the health probe's; the decision client has its own.
	httpClient *http.Client
}

// errTooSmall is the refusal a machine gets when the model would not fit.
var errTooSmall = errors.New("the local decision model needs more memory than this machine has")

// tooSmall reports that this machine has not the memory to load the model,
// and how little it has. A machine that cannot answer the question is never
// refused: unknown is not the same as small.
func tooSmall() (int, bool) {
	have, ok := availableMB()
	if !ok || have >= minAvailableMB {
		return have, false
	}
	return have, true
}

// errNeverReady marks the one failure worth counting: a model that started,
// held the machine for LoadTimeout, and never answered.
var errNeverReady = errors.New("the local decision model did not become ready")

// New builds the backend. It starts nothing: Start does that, so a caller can
// construct one and decide later whether this install wants it running.
func New(cfg Config, home string) *Backend {
	b := &Backend{
		cfg:        cfg,
		home:       home,
		client:     newClient(cfg.BaseURL()),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	b.down.Store("not started")
	b.limits.Store(decision.Limits{})
	return b
}

// Name implements decision.Backend.
func (b *Backend) Name() string { return "laya-" + Checkpoint }

// Healthy reports whether the server is answering right now.
func (b *Backend) Healthy() bool { return b != nil && b.healthy.Load() }

// Down says why it is not, or "" when it is fine.
func (b *Backend) Down() string {
	if b == nil {
		return "not configured"
	}
	reason, _ := b.down.Load().(string)
	return reason
}

func (b *Backend) setDown(format string, args ...any) {
	b.down.Store(fmt.Sprintf(format, args...))
}

// Limits implements decision.Limited: what the checkpoint that actually
// loaded can be asked in one request. Zero until the first health probe
// answers, which every caller reads as "no limit known".
func (b *Backend) Limits() decision.Limits {
	if b == nil {
		return decision.Limits{}
	}
	l, _ := b.limits.Load().(decision.Limits)
	return l
}

// Decide implements decision.Backend. A server that is not up is reported as
// unavailable rather than dialled: the caller's fallback is the point, and a
// connection refused on every decision is a slow way to reach it.
func (b *Backend) Decide(ctx context.Context, req *decision.Request) (*decision.Response, error) {
	// A decision was actually asked for, which is what makes the model worth
	// installing on a machine that has not got it yet. This one falls back;
	// the supervisor picks the permission up on its next attempt.
	b.installOK.Store(true)
	if !b.healthy.Load() {
		return nil, fmt.Errorf("%w: the local decision model is not running (%s)", decision.ErrUnavailable, b.Down())
	}
	return b.client.decide(ctx, req)
}

// Provision permits the install and starts it now rather than on the first
// decision, so a gateway coming up on a fresh machine has the model ready
// before anything asks — the same thing browser.ProvisionInBackground does
// for the headless engine, and for the same reason: a daemon is long enough
// lived to finish a download that a one-shot command would kill.
func (b *Backend) Provision(ctx context.Context) {
	if b == nil || !b.cfg.autoInstall() || b.cfg.Command != "" {
		return
	}
	b.installOK.Store(true)
	if _, ok := FindPython(b.home); ok {
		return // already here; the supervisor starts it
	}
	go func() {
		if _, _, err := b.ensureLaya(ctx, func(format string, args ...any) {
			slog.Info("decision model: " + fmt.Sprintf(format, args...))
		}); err != nil {
			slog.Warn("the local decision model could not be installed; decisions fall back to the agent's own path until it is",
				"error", err)
			return
		}
		slog.Info("the local decision model is installed")
	}()
}

// Start brings the supervisor up. It returns immediately; the first decisions
// while the checkpoint loads fall back, which is what fallback is for.
func (b *Backend) Start(parent context.Context) {
	if b == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	b.cancel = cancel
	b.wg.Add(1)
	go b.run(ctx)
}

// Stop ends the supervisor and waits for the child to go.
func (b *Backend) Stop() {
	if b == nil {
		return
	}
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
}

func (b *Backend) run(ctx context.Context) {
	defer b.wg.Done()
	// A stopped supervisor is not a healthy model. Without this the flag
	// outlives the child, and the next decision dials a port nothing is
	// listening on instead of falling back the way it should.
	defer func() {
		b.healthy.Store(false)
		if ctx.Err() != nil {
			b.setDown("the local decision model was stopped")
		}
	}()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		if b.probe(ctx) == nil {
			b.healthy.Store(true)
			b.down.Store("")
			backoff = 5 * time.Second
			b.neverReady = 0
			b.pollWhileHealthy(ctx)
			continue
		}
		b.healthy.Store(false)
		err := b.spawnAndWait(ctx)
		if ctx.Err() != nil {
			return
		}
		if b.spent(err) {
			return
		}
		slog.Warn("the local decision model exited; restarting", "error", err, "backoff", backoff)
		sleepCtx(ctx, backoff)
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// spent records one supervised run and reports whether this machine has
// failed to load the model often enough to stop trying.
//
// A model that starts and then never finishes loading is the expensive
// failure: each attempt is LoadTimeout of solid arithmetic, and on a machine
// slow enough to hit it that is the whole machine. Retrying that forever is
// how a box ends up being power-cycled — so the supervisor stops, says so,
// and names the setting that keeps it stopped. Every other failure is cheap
// to retry and is left to the backoff.
func (b *Backend) spent(err error) bool {
	if !errors.Is(err, errNeverReady) {
		return false
	}
	if b.neverReady++; b.neverReady < loadAttempts {
		return false
	}
	b.setDown("the local decision model could not finish loading on this machine in %d attempts; decisions fall back, and decision.mode: off stops it trying", loadAttempts)
	slog.Warn("the local decision model could not finish loading on this machine; giving up for this run",
		"attempts", loadAttempts, "remedy", "decision.mode: off")
	return true
}

func (b *Backend) pollWhileHealthy(ctx context.Context) {
	for ctx.Err() == nil {
		sleepCtx(ctx, b.reprobeInterval())
		if ctx.Err() != nil {
			return
		}
		if b.probe(ctx) != nil {
			b.healthy.Store(false)
			b.setDown("the local decision model stopped answering")
			return
		}
	}
}

func (b *Backend) reprobeInterval() time.Duration {
	if b.probeInterval > 0 {
		return b.probeInterval
	}
	return probeEvery
}

// health is what the server reports about itself.
type health struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	MaxLen     int    `json:"max_len"`
	HeadMaxLen int    `json:"head_max_len"`
}

// probe asks the server whether it is up, and records the limits it reports.
// serverCommand turns the interpreter the install resolved into the command
// that serves. A configured decision.local.command is taken as the whole
// thing — someone naming their own runtime means it, and it may not live in
// a virtualenv with a console script beside it.
func (b *Backend) serverCommand(resolved string) string {
	if b.cfg.Command != "" {
		return resolved
	}
	return ServerBin(b.home)
}

// serveArgs is how the runtime is told what to serve and how much of the
// machine it may use. --threads is the runtime's own cap, set alongside the
// environment's: onnxruntime reads one and the thread pool the other, and a
// box with two slow cores needs both.
func (b *Backend) serveArgs() []string {
	args := []string{
		"serve",
		"--model", ModelDir(b.home),
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(b.cfg.port()),
		"--threads", strconv.Itoa(computeThreads()),
	}
	if provider := strings.TrimSpace(b.cfg.Device); provider != "" {
		args = append(args, "--provider", provider)
	}
	return args
}

func (b *Backend) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.BaseURL()+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var h health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return err
	}
	if !h.OK {
		if h.Error != "" {
			return fmt.Errorf("the local decision model is not ready: %s", h.Error)
		}
		return fmt.Errorf("the local decision model is still loading")
	}
	b.limits.Store(b.windowLimits(h))
	return nil
}

// windowLimits answers what a caller may send. The server reports whether it
// is up but not the window it was built with, so the window comes from the
// model's own metadata, which is on disk beside it and cannot disagree with
// the graph that was unpacked with it. A health probe that does carry the
// numbers is believed over the file, so a server that starts reporting them
// needs no change here.
func (b *Backend) windowLimits(h health) decision.Limits {
	if h.MaxLen > 0 {
		return limitsOf(h)
	}
	if cfg, ok := readModelConfig(b.home); ok {
		return limitsOf(health{MaxLen: cfg.MaxLen, HeadMaxLen: cfg.HeadMaxLen})
	}
	return decision.Limits{}
}

// limitsOf turns the server's token budgets into the character and candidate
// budgets a caller can size a request against. Both are estimates and are
// meant to be: what they replace is a request that is silently truncated
// inside the model, or refused outright for a question head that does not
// fit, neither of which the caller can see.
func limitsOf(h health) decision.Limits {
	l := decision.Limits{}
	if h.MaxLen > 0 && h.HeadMaxLen > 0 && h.MaxLen > h.HeadMaxLen {
		l.MaxStateChars = (h.MaxLen - h.HeadMaxLen) * charsPerToken
	}
	if h.HeadMaxLen > 0 {
		l.MaxCandidates = min(h.HeadMaxLen/tokensPerOption, maxOfferedCandidates)
	}
	return l
}

// serverEnv is the environment the server is born with. Hugging Face's hub
// reports usage as it resolves a repository, which a personal agent's sidecar
// has no business doing; the switch has to be in the environment before the
// interpreter starts, which is why it lives here rather than in the script.
func serverEnv() []string {
	return append(os.Environ(), "HF_HUB_DISABLE_TELEMETRY=1", "TRANSFORMERS_NO_ADVISORY_WARNINGS=1",
		// onnxruntime runs its CPU kernels on one thread per core, and the
		// warm-up holds every one of them for as long as the checkpoint takes
		// to build. On a two-core box that is the whole machine: measured, a
		// Factor whose decision model was loading stopped answering ssh and
		// then stopped answering at all. A core is left for everything else
		// — Factor, the memory engine, and whoever is trying to log in and
		// find out what is wrong. The variables have to be in the
		// environment before the interpreter starts, which is why they are
		// set here rather than in the script.
		"OMP_NUM_THREADS="+strconv.Itoa(computeThreads()),
		"MKL_NUM_THREADS="+strconv.Itoa(computeThreads()))
}

// computeThreads is how many cores the model may hold at once: all but one,
// and at least one.
func computeThreads() int {
	if n := runtime.NumCPU() - 1; n > 0 {
		return n
	}
	return 1
}

func (b *Backend) spawnAndWait(ctx context.Context) error {
	if have, small := tooSmall(); small {
		err := fmt.Errorf("%w: this machine has %d MB free and the model needs about %d MB to load",
			errTooSmall, have, minAvailableMB)
		b.setDown("%v; decisions fall back, and decision.mode: off stops it trying", err)
		return err
	}
	command, err := b.resolveCommand(ctx)
	if err != nil {
		b.setDown("%v", err)
		return err
	}
	b.down.Store("")

	cmd := exec.CommandContext(ctx, b.serverCommand(command), b.serveArgs()...)
	cmd.Env = serverEnv()
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { childproc.Stop(cmd.Process); return nil }

	logDir := filepath.Join(b.home, "logs")
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		if f, ferr := os.OpenFile(filepath.Join(logDir, "decision-server.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
			defer f.Close()
			cmd.Stdout, cmd.Stderr = f, f
		}
	}

	if err := cmd.Start(); err != nil {
		b.setDown("could not start the local decision model: %v", err)
		return err
	}
	// Niced after the start, which is the only place pure Go can do it: even
	// held to one core short of the machine, the warm-up is minutes of solid
	// arithmetic, and nothing else here is worth making wait behind it.
	lowerPriority(cmd.Process.Pid)
	slog.Info("local decision model starting", "port", b.cfg.port(), "pid", cmd.Process.Pid)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// The first run downloads the weights, so the wait for a first health
	// answer is long; every run after it is seconds.
	deadline := time.Now().Add(LoadTimeout)
	for {
		select {
		case err := <-waitCh:
			b.setDown("the local decision model exited: %v (see %s)", err, filepath.Join(logDir, "decision-server.log"))
			return err
		case <-ctx.Done():
			return childproc.StopAndWait(cmd.Process, waitCh, stopGrace)
		case <-time.After(time.Second):
		}
		if b.probe(ctx) == nil {
			b.healthy.Store(true)
			b.down.Store("")
			slog.Info("local decision model ready", "limits", b.Limits())
			break
		}
		if time.Now().After(deadline) {
			b.setDown("the local decision model did not become ready within %s", LoadTimeout)
			_ = childproc.StopAndWait(cmd.Process, waitCh, stopGrace)
			return fmt.Errorf("%w within %s", errNeverReady, LoadTimeout)
		}
	}

	// Healthy: stay with the child, and keep asking. Waiting on the process
	// alone is not enough — a model that wedges (a deadlock, a machine
	// thrashing) keeps its process and would stay marked healthy forever,
	// which costs every decision its whole timeout instead of an immediate
	// fallback. So the probe continues for as long as the child runs, and a
	// model that stops answering is stopped and started again.
	for {
		select {
		case err := <-waitCh:
			b.healthy.Store(false)
			b.setDown("the local decision model exited: %v", err)
			return err
		case <-ctx.Done():
			return childproc.StopAndWait(cmd.Process, waitCh, stopGrace)
		case <-time.After(b.reprobeInterval()):
			if b.probe(ctx) != nil {
				b.healthy.Store(false)
				b.setDown("the local decision model stopped answering; restarting it")
				slog.Warn("the local decision model stopped answering; restarting it")
				return childproc.StopAndWait(cmd.Process, waitCh, stopGrace)
			}
		}
	}
}

// resolveCommand locates the interpreter, installing Laya when it is missing.
// The install is attempted at most once per process: a machine that cannot
// install must not re-run a long download on every restart.
func (b *Backend) resolveCommand(ctx context.Context) (string, error) {
	if b.cfg.Command != "" {
		return resolveInterpreter(b.cfg.Command)
	}
	if path, ok := FindPython(b.home); ok {
		return path, nil
	}
	if !b.cfg.autoInstall() {
		return "", fmt.Errorf("the local decision model is not installed and decision.auto_install is off — %s", InstallHint())
	}
	if !b.installOK.Load() {
		// Nothing has asked for a decision yet and no gateway has said it
		// will wait for the download. Reported rather than begun.
		return "", fmt.Errorf("the local decision model is not installed yet; the gateway installs it in the background, as does the first decision asked for")
	}
	if b.installTried.Swap(true) {
		return "", fmt.Errorf("the local decision model is not installed and the automatic install already failed this run")
	}
	slog.Info("the local decision model is missing; installing it", "spec", PackageSpec)
	path, _, err := b.ensureLaya(ctx, func(format string, args ...any) {
		slog.Info("decision install: " + fmt.Sprintf(format, args...))
	})
	return path, err
}

// ensureLaya installs the model, one caller at a time. EnsureLaya answers from
// the virtualenv when there is one, so whoever waits here finds the work done
// rather than repeating it.
func (b *Backend) ensureLaya(ctx context.Context, progress func(string, ...any)) (string, bool, error) {
	b.installMu.Lock()
	defer b.installMu.Unlock()
	return EnsureLaya(ctx, b.home, b.cfg.Device, true, progress)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
