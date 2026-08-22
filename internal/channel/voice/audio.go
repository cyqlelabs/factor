package voice

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	// PlayFile plays one whole WAV clip through a helper that can only read
	// files (afplay). Cancelling ctx kills it mid-note.
	PlayFile func(ctx context.Context, argv []string, wav []byte) error
	// Run executes one short-lived helper and returns its stdout — device
	// listing, not audio.
	Run func(ctx context.Context, argv ...string) (string, error)
}

// DefaultEnv wires Env to the real machine.
func DefaultEnv() Env {
	return Env{
		Has:      hasBinary,
		Getenv:   os.Getenv,
		Glob:     filepath.Glob,
		GOOS:     runtime.GOOS,
		Capture:  captureExec,
		Play:     playExec,
		PlayFile: playFileExec,
		Run:      runExec,
	}
}

func runExec(ctx context.Context, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	return string(out), err
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

// playFileExec hands the clip to a file-only helper: written to a temp file,
// played, removed. The helper gets the file path as its last argument.
func playFileExec(ctx context.Context, argv []string, wav []byte) error {
	f, err := os.CreateTemp("", "factor-voice-*.wav")
	if err != nil {
		return err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := f.Write(wav); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], path)...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
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
	case e.has("afplay"):
		// macOS ships afplay; it only reads files, so the player hands it
		// whole clips instead of a stream. Last on the list because the
		// streaming helpers pause with better precision.
		return []string{"afplay"}, nil
	}
	return nil, fmt.Errorf("no speaker helper is installed (want one of paplay, pw-play, aplay, play, afplay)")
}

// MachineHasAudio reports whether this machine has a sound system at all —
// the same question MachineHasDisplay answers for screens, asked of the audio
// stack: a setup run over ssh sees no PULSE_SERVER while the sound card in
// the box works the whole time, so the devices are probed, not the
// environment.
func MachineHasAudio(e Env) bool {
	// Windows is the one platform with no helper on either side —
	// captureCommand and playbackCommand both refuse — so however good the
	// sound card is, there is none Factor can reach. Answering yes here is
	// what used to make the wizard offer PC voice and configure a channel
	// that could only fail on its first utterance.
	if e.GOOS == "windows" {
		return false
	}
	if e.GOOS == "darwin" {
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

// CaptureSources names the machine's capture devices, for the wizard's
// microphone menu. Monitor sources — outputs echoed back as inputs — are left
// out: pointing the agent's ear at its own mouth is how it talks to itself.
// Only the PulseAudio interface is asked (PipeWire answers it too); a machine
// without pactl gets no menu and uses the default source.
func CaptureSources(ctx context.Context, e Env) []string {
	if e.Run == nil || !e.has("pactl") {
		return nil
	}
	out, err := e.Run(ctx, "pactl", "list", "sources", "short")
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// A source row is "<index> <name> …" — anything else is noise.
		if len(fields) < 2 || strings.HasSuffix(fields[1], ".monitor") {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue
		}
		names = append(names, fields[1])
	}
	return names
}

// MeasureMic captures from one device for d and reports the loudest frame —
// the wizard's live check. A peak of exactly 0 is the wrong-device signature:
// a real microphone always carries noise.
func MeasureMic(ctx context.Context, e Env, device string, d time.Duration) (float64, error) {
	argv, err := captureCommand(e, device)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, d+5*time.Second)
	defer cancel()
	stream, err := e.Capture(ctx, argv)
	if err != nil {
		return 0, err
	}
	defer stream.Close()

	deadline := time.Now().Add(d)
	frame := make([]byte, frameBytes)
	var peak float64
	for time.Now().Before(deadline) {
		if _, err := io.ReadFull(stream, frame); err != nil {
			return peak, fmt.Errorf("the capture stream ended early: %w", err)
		}
		if level := rms(frame); level > peak {
			peak = level
		}
	}
	return peak, nil
}
