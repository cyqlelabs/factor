package voice

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/desktop"
)

// Audio goes through the sound system's own helper programs (parec/paplay on
// PulseAudio and PipeWire, arecord/aplay on bare ALSA, sox's rec/play on
// macOS) rather than CGO bindings, for the same reason the desktop tools do:
// Factor stays a single static binary. Capture streams raw PCM from a helper's
// stdout; playback feeds raw PCM to a helper's stdin, so killing the helper is
// what makes barge-in instant.

const (
	// captureRate is what the transcribers expect; playbackRate is what every
	// synthesiser here emits (the managed server's contract, ElevenLabs'
	// pcm_24000, OpenAI's "pcm"). Both are 16-bit little-endian mono.
	captureRate  = 16000
	playbackRate = 24000
)

// Env is the seam between the channel and the machine's sound system. Tests
// substitute scripted functions.
type Env struct {
	Has    func(bin string) bool
	Getenv func(key string) string
	Glob   func(pattern string) ([]string, error)
	GOOS   string

	// Capture starts a helper that writes raw PCM to its stdout and returns
	// the stream; Close kills the helper.
	Capture func(ctx context.Context, argv []string) (io.ReadCloser, error)
	// Play runs a helper that reads raw PCM from stdin until pcm drains or
	// ctx is cancelled, which kills it mid-note.
	Play func(ctx context.Context, argv []string, pcm io.Reader) error
}

// DefaultEnv wires Env to the real machine.
func DefaultEnv() Env {
	return Env{
		Has:     hasBinary,
		Getenv:  os.Getenv,
		Glob:    filepath.Glob,
		GOOS:    runtime.GOOS,
		Capture: captureExec,
		Play:    playExec,
	}
}

func (e Env) has(bin string) bool {
	if e.Has == nil {
		return false
	}
	return e.Has(bin)
}

func (e Env) env(key string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(key)
}

func hasBinary(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// captureExec starts the helper and hands back its stdout. Stderr is dropped:
// the helpers narrate latency and buffer sizes there, and none of it belongs
// in a PCM stream or a log.
func captureExec(ctx context.Context, argv []string) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &procStream{Reader: stdout, stop: func() {
		cancel()
		_ = cmd.Wait()
	}}, nil
}

// procStream ties a helper's stdout to its lifetime: Close kills the helper
// and reaps it, after which reads return EOF.
type procStream struct {
	io.Reader
	stop func()
	once sync.Once
}

func (p *procStream) Close() error {
	p.once.Do(p.stop)
	return nil
}

func playExec(ctx context.Context, argv []string, pcm io.Reader) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = pcm
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		// Killed on purpose — a barge-in or shutdown, not a failure.
		return ctx.Err()
	}
	return err
}

// captureCommand builds the helper invocation that streams 16 kHz s16le mono
// from the microphone to stdout. Preference order: the PulseAudio interface
// first (PipeWire serves it too), then PipeWire's own tool, bare ALSA, and
// sox — which is also the practical choice on macOS, where AUDIODEV selects
// the device.
func captureCommand(e Env, device string) ([]string, error) {
	if e.GOOS == "windows" {
		return nil, fmt.Errorf("microphone capture is not supported on Windows yet")
	}
	switch {
	case e.has("parec"):
		argv := []string{"parec", "--format=s16le", "--rate=16000", "--channels=1", "--latency-msec=30"}
		if device != "" {
			argv = append(argv, "--device="+device)
		}
		return argv, nil
	case e.has("pw-record"):
		argv := []string{"pw-record", "--format=s16", "--rate=16000", "--channels=1"}
		if device != "" {
			argv = append(argv, "--target", device)
		}
		return append(argv, "-"), nil
	case e.has("arecord"):
		argv := []string{"arecord", "-q", "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "raw"}
		if device != "" {
			argv = append(argv, "-D", device)
		}
		return argv, nil
	case e.has("rec"):
		return []string{"rec", "-q", "-t", "raw", "-b", "16", "-e", "signed-integer",
			"-r", "16000", "-c", "1", "-"}, nil
	}
	return nil, fmt.Errorf("no microphone helper is installed (want one of parec, pw-record, arecord, rec)")
}

// playbackCommand builds the helper invocation that plays 24 kHz s16le mono
// from stdin.
func playbackCommand(e Env, device string) ([]string, error) {
	if e.GOOS == "windows" {
		return nil, fmt.Errorf("speaker playback is not supported on Windows yet")
	}
	switch {
	case e.has("paplay"):
		argv := []string{"paplay", "--raw", "--format=s16le", "--rate=24000", "--channels=1"}
		if device != "" {
			argv = append(argv, "--device="+device)
		}
		return argv, nil
	case e.has("pw-play"):
		argv := []string{"pw-play", "--format=s16", "--rate=24000", "--channels=1"}
		if device != "" {
			argv = append(argv, "--target", device)
		}
		return append(argv, "-"), nil
	case e.has("aplay"):
		argv := []string{"aplay", "-q", "-f", "S16_LE", "-r", "24000", "-c", "1", "-t", "raw"}
		if device != "" {
			argv = append(argv, "-D", device)
		}
		return argv, nil
	case e.has("play"):
		return []string{"play", "-q", "-t", "raw", "-b", "16", "-e", "signed-integer",
			"-r", "24000", "-c", "1", "-"}, nil
	}
	return nil, fmt.Errorf("no speaker helper is installed (want one of paplay, pw-play, aplay, play)")
}

// MachineHasAudio reports whether this machine has a sound system at all —
// the same question MachineHasDisplay answers for screens, asked of the audio
// stack: a setup run over ssh sees no PULSE_SERVER while the sound card in
// the box works the whole time, so the devices are probed, not the
// environment.
func MachineHasAudio(e Env) bool {
	if e.GOOS == "darwin" || e.GOOS == "windows" {
		return true
	}
	if e.Glob == nil {
		return false
	}
	if m, err := e.Glob("/dev/snd/pcm*"); err == nil && len(m) > 0 {
		return true
	}
	if dir := e.env("XDG_RUNTIME_DIR"); dir != "" {
		for _, socket := range []string{"pipewire-0", filepath.Join("pulse", "native")} {
			if m, err := e.Glob(filepath.Join(dir, socket)); err == nil && len(m) > 0 {
				return true
			}
		}
	}
	return false
}

// The helpers the wizard installs when a machine has a sound card but no way
// to talk to it. One package usually carries both directions.
var (
	captureHelper = desktop.Helper{Bin: "parec", Purpose: "microphone capture",
		Packages: map[string]string{"apt": "pulseaudio-utils", "dnf": "pulseaudio-utils",
			"pacman": "libpulse", "apk": "pulseaudio-utils", "xbps": "pulseaudio-utils"}}
	playbackHelper = desktop.Helper{Bin: "paplay", Purpose: "speaker playback",
		Packages: map[string]string{"apt": "pulseaudio-utils", "dnf": "pulseaudio-utils",
			"pacman": "libpulse", "apk": "pulseaudio-utils", "xbps": "pulseaudio-utils"}}
)

// MissingHelpers lists what the channel needs and cannot find; empty means
// both directions of audio have a helper.
func MissingHelpers(e Env) []desktop.Helper {
	var missing []desktop.Helper
	if _, err := captureCommand(e, ""); err != nil {
		missing = append(missing, captureHelper)
	}
	if _, err := playbackCommand(e, ""); err != nil {
		missing = append(missing, playbackHelper)
	}
	return missing
}
