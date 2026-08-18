package voice

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// scriptedEnv builds an Env whose machine has exactly the named binaries and
// files; Capture and Play are left nil for the command-selection tests.
func scriptedEnv(goos string, installed ...string) Env {
	bins := map[string]bool{}
	for _, b := range installed {
		bins[b] = true
	}
	return Env{
		GOOS:   goos,
		Has:    func(bin string) bool { return bins[bin] },
		Getenv: func(string) string { return "" },
		Glob:   func(string) ([]string, error) { return nil, nil },
	}
}

func TestCaptureCommandPrefersPulseThenPipewireThenALSA(t *testing.T) {
	cases := []struct {
		installed []string
		wantBin   string
	}{
		{[]string{"parec", "pw-record", "arecord", "rec"}, "parec"},
		{[]string{"pw-record", "arecord", "rec"}, "pw-record"},
		{[]string{"arecord", "rec"}, "arecord"},
		{[]string{"rec"}, "rec"},
	}
	for _, tc := range cases {
		argv, err := captureCommand(scriptedEnv("linux", tc.installed...), "")
		if err != nil {
			t.Fatalf("%v: %v", tc.installed, err)
		}
		if argv[0] != tc.wantBin {
			t.Errorf("with %v the helper is %q, want %q", tc.installed, argv[0], tc.wantBin)
		}
		line := strings.Join(argv, " ")
		if !strings.Contains(line, "16000") {
			t.Errorf("%q does not ask for 16 kHz", line)
		}
	}
}

func TestCaptureCommandPassesTheDevice(t *testing.T) {
	for _, bin := range []string{"parec", "pw-record", "arecord"} {
		argv, err := captureCommand(scriptedEnv("linux", bin), "front-mic")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(argv, " "), "front-mic") {
			t.Errorf("%s invocation lost the device: %v", bin, argv)
		}
	}
}

func TestPlaybackCommandSelection(t *testing.T) {
	cases := []struct {
		installed []string
		wantBin   string
	}{
		{[]string{"paplay", "pw-play", "aplay", "play"}, "paplay"},
		{[]string{"pw-play", "aplay"}, "pw-play"},
		{[]string{"aplay"}, "aplay"},
		{[]string{"play"}, "play"},
	}
	for _, tc := range cases {
		argv, err := playbackCommand(scriptedEnv("linux", tc.installed...), "")
		if err != nil {
			t.Fatalf("%v: %v", tc.installed, err)
		}
		if argv[0] != tc.wantBin {
			t.Errorf("with %v the helper is %q, want %q", tc.installed, argv[0], tc.wantBin)
		}
		if !strings.Contains(strings.Join(argv, " "), "24000") {
			t.Errorf("%v does not ask for 24 kHz", argv)
		}
	}
}

func TestAudioCommandsReportWhatIsMissing(t *testing.T) {
	if _, err := captureCommand(scriptedEnv("linux"), ""); err == nil || !strings.Contains(err.Error(), "parec") {
		t.Errorf("capture error = %v, want it to name the helpers", err)
	}
	if _, err := playbackCommand(scriptedEnv("linux"), ""); err == nil || !strings.Contains(err.Error(), "paplay") {
		t.Errorf("playback error = %v, want it to name the helpers", err)
	}
	if _, err := captureCommand(scriptedEnv("windows", "parec"), ""); err == nil {
		t.Error("windows capture should be refused for now")
	}
	if _, err := playbackCommand(scriptedEnv("windows", "paplay"), ""); err == nil {
		t.Error("windows playback should be refused for now")
	}
}

func TestMachineHasAudioProbesDevicesNotTheEnvironment(t *testing.T) {
	// A sound card is visible under /dev/snd whether or not this shell can
	// see a sound server — the ssh lesson, restated for audio.
	env := scriptedEnv("linux")
	env.Glob = func(pattern string) ([]string, error) {
		if pattern == "/dev/snd/pcm*" {
			return []string{"/dev/snd/pcmC0D0p"}, nil
		}
		return nil, nil
	}
	if !MachineHasAudio(env) {
		t.Error("a machine with ALSA devices reported no audio")
	}

	deaf := scriptedEnv("linux")
	if MachineHasAudio(deaf) {
		t.Error("a machine with no devices reported audio")
	}

	// A user session's sound server counts too.
	pulse := scriptedEnv("linux")
	pulse.Getenv = func(key string) string {
		if key == "XDG_RUNTIME_DIR" {
			return "/run/user/1000"
		}
		return ""
	}
	pulse.Glob = func(pattern string) ([]string, error) {
		if pattern == filepath.Join("/run/user/1000", "pulse", "native") {
			return []string{pattern}, nil
		}
		return nil, nil
	}
	if !MachineHasAudio(pulse) {
		t.Error("a machine with a pulse socket reported no audio")
	}

	if !MachineHasAudio(scriptedEnv("darwin")) {
		t.Error("macOS always has CoreAudio")
	}
}

func TestMissingHelpersListsEachDirectionOnce(t *testing.T) {
	missing := MissingHelpers(scriptedEnv("linux"))
	if len(missing) != 2 {
		t.Fatalf("missing = %d helpers, want capture and playback", len(missing))
	}
	if missing[0].Bin != "parec" || missing[1].Bin != "paplay" {
		t.Errorf("missing = %v", missing)
	}
	if missing[0].Package("apt") != "pulseaudio-utils" || missing[0].Package("pacman") != "libpulse" {
		t.Errorf("packages = %v", missing[0].Packages)
	}
	if got := MissingHelpers(scriptedEnv("linux", "arecord", "aplay")); len(got) != 0 {
		t.Errorf("ALSA-only machine reported %v missing", got)
	}
}

// The real executors, driven through /bin/sh so no audio hardware is needed:
// capture streams a helper's stdout and Close kills it; playback feeds stdin
// and a cancelled context kills it mid-stream.
func TestDefaultEnvCaptureStreamsAndCloseKills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sh on windows")
	}
	env := DefaultEnv()
	stream, err := env.Capture(context.Background(),
		[]string{"sh", "-c", "printf hello; while :; do sleep 1; done"})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("read %q, %v", buf, err)
	}
	done := make(chan struct{})
	go func() { _ = stream.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not kill the resident helper")
	}
	if _, err := stream.Read(buf); err == nil {
		t.Error("reads after Close should fail")
	}
}

func TestDefaultEnvPlayFeedsStdinAndHonoursCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sh on windows")
	}
	env := DefaultEnv()
	if err := env.Play(context.Background(), []string{"sh", "-c", "cat >/dev/null"},
		strings.NewReader("pcm-bytes")); err != nil {
		t.Fatalf("a clean playback errored: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	err := env.Play(ctx, []string{"sh", "-c", "cat >/dev/null; sleep 30"}, neverEndingReader{})
	if err == nil {
		t.Error("a cancelled playback reported success")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("cancel did not kill the player promptly")
	}
}

type neverEndingReader struct{}

func (neverEndingReader) Read(b []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	for i := range b {
		b[i] = 0
	}
	return len(b), nil
}
