package upgrade

// The package half of keeping the memory engine current: the smrti that
// `factor init` installed with uv, pipx, pip or a private venv, which is how
// most machines run it. Nothing here is docker's problem — PyPI says what is
// published, the installer that made the install upgrades it, and the engine is
// then stopped so the new code is actually loaded, since a Python process keeps
// the modules it imported however new the files under it are.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
)

// Seams: the package path is testable without a PyPI, an installer, or an
// engine to stop.
var (
	pypiBase         = "https://pypi.org"
	findSmrti        = memory.FindSmrti
	installedVersion = memory.InstalledVersion
	upgradePackage   = memory.Upgrade
	stopEngine       = memory.StopEngineIf
	enginePid        = memory.EnginePid

	// How long a supervisor gets to put a new engine in place of the one that
	// was stopped. The sidecar re-probes a healthy engine every 30 seconds,
	// so that long can pass before it notices the stop at all, and it then
	// waits its backoff before spawning; a slow box was measured at 45
	// seconds between the stop and the new pid. After this, nothing here
	// supervises the engine.
	smrtiRespawnWait = 90 * time.Second

	// How long a stopped engine gets to release its port.
	smrtiPortWait = 5 * time.Second

	// stopBeforeInstall is whether a live engine pins the files its upgrade
	// has to replace. Windows locks a running executable and every extension
	// module a process has loaded, so pip could not replace smrti.exe under
	// the engine — and its failed uninstall left the package renamed to
	// ~mrti with no metadata, which broke memory rather than upgrading it.
	stopBeforeInstall = runtime.GOOS == "windows"
)

// checkPackage reports what PyPI publishes against the smrti installed here.
// noContainer is why the container half found nothing, which is half the answer
// when this half finds nothing either.
func (s *Smrti) checkPackage(ctx context.Context, noContainer error) (SmrtiRelease, error) {
	if !s.localEngine() {
		return SmrtiRelease{}, fmt.Errorf("%w: the engine answers at %s, so it belongs to whoever runs it there",
			ErrNotManaged, s.cfg.BaseURL())
	}
	exe, ok := findSmrti(s.cfg.Command, config.Home())
	if !ok {
		return SmrtiRelease{}, fmt.Errorf("%w: no smrti is installed on this machine, and %v",
			ErrNotManaged, noContainer)
	}
	latest, err := latestSmrtiPackage(ctx)
	if err != nil {
		return SmrtiRelease{}, err
	}
	return SmrtiRelease{
		Mode:    ModePackage,
		Running: s.packageVersion(ctx, exe),
		Version: latest,
		Path:    exe,
	}, nil
}

// packageVersion is the version running here. The engine is asked first — it is
// the code actually serving, which is what an upgrade is measured against — and
// the install on disk answers when the engine is down or too old to say.
func (s *Smrti) packageVersion(ctx context.Context, exe string) string {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := memory.NewClient(s.cfg.BaseURL(), s.cfg.APIKey, "").Status(probeCtx)
	if err == nil {
		if v, ok := status["version"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return installedVersion(ctx, exe)
}

// applyPackage installs the published release over the one here and restarts
// the engine into it. The install goes first, while the old engine keeps
// serving, so the only interruption is the restart — except where the engine
// pins its own files, and there it has to stop first.
func (s *Smrti) applyPackage(ctx context.Context, rel SmrtiRelease, progress Progress) (string, error) {
	if stopBeforeInstall {
		return s.applyStopped(ctx, rel, progress)
	}
	method, err := upgradePackage(ctx, rel.Path, config.Home(), memory.Progress(progress))
	if err != nil {
		return "", err
	}
	progress("installed smrti %s with %s", rel.Version, method)

	note, err := s.restartEngine(ctx, progress)
	if err != nil {
		return "", fmt.Errorf("smrti %s is installed, but %w", rel.Version, err)
	}
	return note, nil
}

// applyStopped is applyPackage where the engine has to be gone before its
// files can be replaced. Whatever engine serves is on the old code, so it is
// stopped whichever it is, and its port is held until the install is done:
// the supervisor waits on a port something holds rather than spawning, which
// keeps it from starting an engine on half-installed files — or from reading
// them as broken and reinstalling over the install in progress.
func (s *Smrti) applyStopped(ctx context.Context, rel SmrtiRelease, progress Progress) (string, error) {
	stopped, hold, err := s.stopAndHold(ctx, progress)
	if err != nil {
		return "", err
	}
	method, err := upgradePackage(ctx, rel.Path, config.Home(), memory.Progress(progress))
	if hold != nil {
		_ = hold.Close()
	}
	if err != nil {
		return "", err
	}
	progress("installed smrti %s with %s", rel.Version, method)
	if stopped == 0 {
		return "no engine is running here, so it loads the next time the engine starts", nil
	}
	note, err := s.awaitReplacement(ctx, stopped, progress)
	if err != nil {
		return "", fmt.Errorf("smrti %s is installed, but %w", rel.Version, err)
	}
	return note, nil
}

// stopAndHold stops whatever engine serves once the graph is quiet and takes
// its port, returning the pid it stopped and the held port — zero and nil
// when no engine was running.
func (s *Smrti) stopAndHold(ctx context.Context, progress Progress) (int, net.Listener, error) {
	if err := s.waitIdle(ctx, progress); err != nil {
		return 0, nil, err
	}
	progress("stopping the memory engine, which holds its own files open while it runs")
	stopped, err := stopEngine(ctx, s.port(), memory.AnyEngine)
	if err != nil {
		return 0, nil, fmt.Errorf("the engine running here could not be stopped: %w", err)
	}
	if stopped == 0 {
		return 0, nil, nil
	}
	// A port still taken is an engine still running — one the stop did not
	// reach, or a replacement already spawned — and installing under it is
	// the failure this order exists to avoid.
	hold, err := holdPort(ctx, s.address())
	if err != nil {
		return 0, nil, fmt.Errorf("the engine was stopped, but its port is still taken, so nothing was installed over files it may hold open: %w", err)
	}
	progress("holding the memory port while the package installs, so the supervisor waits for it")
	return stopped, hold, nil
}

// holdPort listens on the engine's address once the stopped engine has let go
// of it, which takes a moment after the process is gone.
func holdPort(ctx context.Context, addr string) (net.Listener, error) {
	deadline := time.Now().Add(smrtiPortWait)
	for {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			go refuse(l)
			return l, nil
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, 200*time.Millisecond) {
			return nil, err
		}
	}
}

// refuse accepts and drops every connection on a held port, so a health probe
// fails at once instead of waiting out its timeout against a listener that
// never answers.
func refuse(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// Restart stops the engine serving here and waits for it to come back on the
// code already installed. It is the whole of what "restart your memory" needs,
// and the reason it exists is what happened without it: told an upgrade would
// load "the next time the engine starts", the agent stopped and started the
// engine by hand through exec, and the engine it started ran outside the
// supervisor, without the environment the supervisor sets — the wrong memory
// space and no cap on its allocator — until it had eaten the machine.
func (s *Smrti) Restart(ctx context.Context, progress Progress) (string, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	if !s.localEngine() {
		return "", fmt.Errorf("%w: the engine answers at %s, so it belongs to whoever runs it there",
			ErrNotManaged, s.cfg.BaseURL())
	}
	c, err := s.container(ctx)
	switch {
	case errors.Is(err, errNoContainer):
		return s.restartEngine(ctx, progress)
	case err != nil:
		return "", err
	}
	if err := s.waitIdle(ctx, progress); err != nil {
		return "", err
	}
	progress("restarting %s", c.name())
	if _, err := dockerCmd(ctx, "restart", "-t", smrtiStopTimeout, c.name()); err != nil {
		return "", err
	}
	if err := s.waitHealthy(ctx); err != nil {
		return "", err
	}
	return "the container restarted and is answering again", nil
}

// restartEngine stops the engine on the memory port once the graph is quiet
// and waits for its supervisor to bring it back, reporting in one clause what
// became of it.
func (s *Smrti) restartEngine(ctx context.Context, progress Progress) (string, error) {
	// Which engine is running the old code, read before the wait rather than
	// after it: waiting for a quiet graph can take minutes, and an engine
	// that restarted during them started after the install and is already on
	// the new code. Stopping that one is downtime for nothing.
	before, _ := enginePid()
	if err := s.waitIdle(ctx, progress); err != nil {
		return "", err
	}
	progress("restarting the memory engine")
	stopped, err := stopEngine(ctx, s.port(), before)
	if err != nil {
		return "", fmt.Errorf("the engine running here could not be stopped: %w", err)
	}
	if stopped == 0 {
		if before != 0 {
			// It was replaced while the graph was being waited on, which is
			// the restart this was going to perform.
			return "the engine restarted while the graph was settling, so it is already on the new code", nil
		}
		return "no engine is running here, so it loads the next time the engine starts", nil
	}
	return s.awaitReplacement(ctx, stopped, progress)
}

// awaitReplacement waits for a supervisor to bring back the engine that was
// stopped, reporting in one clause what became of it.
func (s *Smrti) awaitReplacement(ctx context.Context, stopped int, progress Progress) (string, error) {
	respawned := s.waitRespawn(ctx, stopped)
	if ctx.Err() != nil {
		return "", fmt.Errorf("the old engine was stopped, but the restart was interrupted: %w", ctx.Err())
	}
	if !respawned {
		// `factor upgrade` in a terminal on a machine with no daemon running:
		// the engine that was warm from an earlier session is now stopped and
		// nobody is going to restart it, which is not a failure — the next
		// Factor to want memory spawns what is installed.
		return "the engine was stopped and nothing here supervises it, so it starts with the next factor run", nil
	}
	progress("waiting for the memory engine to answer")
	if err := s.waitHealthy(ctx); err != nil {
		return "", fmt.Errorf("the engine was restarted, but it is not answering: %w", err)
	}
	return "the engine restarted", nil
}

// port is the one the engine serves on, read off the same URL every request
// goes to.
func (s *Smrti) port() int {
	if u, err := url.Parse(s.cfg.BaseURL()); err == nil {
		if p, err := strconv.Atoi(u.Port()); err == nil {
			return p
		}
	}
	return s.cfg.Port
}

// address is the host and port the engine serves on, read off the same URL.
func (s *Smrti) address() string {
	host := "127.0.0.1"
	if u, err := url.Parse(s.cfg.BaseURL()); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return net.JoinHostPort(host, strconv.Itoa(s.port()))
}

// waitRespawn reports whether something put a new engine in place of the one
// that was stopped. A supervisor writes the replacement's pid down as it spawns
// it, so a pid that is neither the old one nor absent is the signal — and an
// engine that answers is one too, whatever wrote it down.
func (s *Smrti) waitRespawn(ctx context.Context, stopped int) bool {
	deadline := time.Now().Add(smrtiRespawnWait)
	for {
		if pid, alive := enginePid(); alive && pid != stopped {
			return true
		}
		if s.engineAnswers(ctx) {
			return true
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, smrtiHealthPoll) {
			return false
		}
	}
}

func (s *Smrti) engineAnswers(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return memory.NewClient(s.cfg.BaseURL(), s.cfg.APIKey, "").CheckHealth(probeCtx) == nil
}

// localEngine reports whether the configured engine runs on this machine, which
// is what makes the smrti installed here the one that answers. An engine
// somewhere else is not this machine's to upgrade, and installing over the
// local files would report an upgrade that changed nothing.
func (s *Smrti) localEngine() bool {
	u, err := url.Parse(s.cfg.BaseURL())
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// latestSmrtiPackage returns the newest smrti PyPI publishes. Its own metadata
// names it: `info.version` is the latest release, pre-releases excluded, which
// is exactly what pip would install.
func latestSmrtiPackage(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := getJSON(ctx, pypiBase+"/pypi/"+memory.PackageName+"/json", "", &body); err != nil {
		return "", fmt.Errorf("looking up the published smrti releases: %w", err)
	}
	version := strings.TrimSpace(body.Info.Version)
	if _, ok := parseVersion(version); !ok {
		return "", fmt.Errorf("pypi publishes no versioned smrti release (it names %q)", version)
	}
	return version, nil
}
