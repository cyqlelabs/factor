package phone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSpeechServer stands in for the Python speech server, echoing the
// configuration it was handed so tests can assert on what Factor decided.
func fakeSpeechServer() {
	var cfg speechServerConfig
	if err := json.Unmarshal([]byte(os.Getenv("FACTOR_SPEECH_CONFIG")), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fake speech server: bad config:", err)
		os.Exit(4)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "config": cfg})
	})
	go func() { time.Sleep(2 * time.Minute); os.Exit(0) }()
	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)),
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	_ = srv.ListenAndServe()
}

// plantVoice puts a voice's weights "on disk", so the supervisor sees a
// prepared machine rather than something to download.
func plantVoice(t *testing.T, home, voice string) {
	t.Helper()
	dir := filepath.Join(home, "speech", "piper")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, voice+".onnx"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// speechSupervisorFor builds a supervisor around the fake server, with the
// engines already "installed" and the voice's weights on disk so neither
// install path is exercised here.
func speechSupervisorFor(t *testing.T, mode string, mutate func(*SpeechConfig)) (*speechSupervisor, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FACTOR_TEST_SPEECH_MODE", mode)

	cfg := SpeechConfig{
		Port:         freePort(t),
		Command:      os.Args[0],
		WhisperModel: "base",
		PiperVoice:   "es_ES-davefx-medium",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	plantVoice(t, home, cfg.PiperVoice)
	s := newSpeechSupervisor(cfg, home, "es", "test-token", true, true)
	s.probeInterval = 50 * time.Millisecond
	t.Cleanup(s.stop)
	return s, home
}

// A voice named in the config but absent from disk — the wizard's picker, or
// a hand edit — must trigger the download instead of crash-looping the server.
func TestSpeechSupervisorPreparesAConfiguredVoiceMissingFromDisk(t *testing.T) {
	home := t.TempDir()
	cfg := SpeechConfig{WhisperModel: "base", PiperVoice: "es_AR-daniela-high"}
	s := newSpeechSupervisor(cfg, home, "es", "tok", true, true)
	if !s.needsPrepare() {
		t.Error("a voice with no weights on disk was treated as prepared")
	}
	plantVoice(t, home, "es_AR-daniela-high")
	if s.needsPrepare() {
		t.Error("a voice with weights on disk was re-prepared")
	}

	// The other reasons to prepare still hold.
	unchosen := newSpeechSupervisor(SpeechConfig{PiperVoice: "es_AR-daniela-high"}, home, "es", "tok", true, true)
	if !unchosen.needsPrepare() {
		t.Error("an unchosen whisper model was treated as prepared")
	}
	sttOnly := newSpeechSupervisor(SpeechConfig{WhisperModel: "base"}, home, "es", "tok", true, false)
	if sttOnly.needsPrepare() {
		t.Error("a transcription-only tier wanted a voice download")
	}
}

func TestSpeechSupervisorSpawnsTheServerAndPassesItsConfiguration(t *testing.T) {
	s, home := speechSupervisorFor(t, "serve", nil)
	s.start(context.Background())

	if !waitFor(t, 10*time.Second, s.Healthy) {
		t.Fatalf("the speech server never became healthy: %s", s.Down())
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", speechPort(s.cfg)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Config speechServerConfig }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	got := body.Config
	if got.Language != "es" || got.PiperVoice != "es_ES-davefx-medium" {
		t.Errorf("the server was configured with %+v", got)
	}
	if !got.NeedSTT || !got.NeedTTS {
		t.Errorf("need_stt=%v need_tts=%v, want both for a fully local tier", got.NeedSTT, got.NeedTTS)
	}
	if got.Token != "test-token" {
		t.Errorf("token = %q, want the boot secret", got.Token)
	}
	if got.Host != "127.0.0.1" {
		t.Errorf("host = %q, want loopback only", got.Host)
	}
	if _, err := os.Stat(speechScriptPath(home)); err != nil {
		t.Errorf("the server script was not written: %v", err)
	}
}

// The configuration carries this boot's secret, so it must reach the child
// through the environment and never through argv, where every process on the
// machine could read it.
func TestSpeechSupervisorNeverPutsSecretsInArgv(t *testing.T) {
	s, _ := speechSupervisorFor(t, "serve", nil)
	s.start(context.Background())
	if !waitFor(t, 10*time.Second, s.Healthy) {
		t.Fatalf("the speech server never became healthy: %s", s.Down())
	}

	out, err := os.ReadFile(filepath.Join("/proc", "self", "cmdline"))
	if err != nil {
		t.Skip("no /proc on this machine")
	}
	if strings.Contains(string(out), "test-token") {
		t.Error("the boot secret appeared in argv")
	}
}

func TestSpeechSupervisorRestartsAServerThatDies(t *testing.T) {
	s, _ := speechSupervisorFor(t, "exit", nil)
	s.start(context.Background())

	if !waitFor(t, 5*time.Second, func() bool { return s.Down() == "" && !s.Healthy() }) {
		t.Log("server exits immediately, as configured")
	}
	if s.Healthy() {
		t.Error("a server that exits at once should never be healthy")
	}
}

// Without an interpreter there is nothing to run, and the reason has to reach
// the user rather than being swallowed by the restart loop.
func TestSpeechSupervisorReportsAMissingInterpreter(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-python")
	s, _ := speechSupervisorFor(t, "serve", func(c *SpeechConfig) { c.Command = missing })
	s.start(context.Background())

	if !waitFor(t, 5*time.Second, func() bool { return s.Down() != "" }) {
		t.Fatal("a missing interpreter was never reported")
	}
	if !strings.Contains(s.Down(), "does not exist") {
		t.Errorf("Down() = %q", s.Down())
	}
	if s.Healthy() {
		t.Error("a supervisor with no interpreter reported itself healthy")
	}
}

// Auto-install off means the user took responsibility, so the supervisor says
// so instead of quietly downloading half a gigabyte.
func TestSpeechSupervisorHonoursAutoInstallOff(t *testing.T) {
	off := false
	home := t.TempDir()
	cfg := SpeechConfig{Port: freePort(t), AutoInstall: &off}
	s := newSpeechSupervisor(cfg, home, "en", "tok", true, true)
	s.probeInterval = 50 * time.Millisecond
	t.Cleanup(s.stop)
	s.start(context.Background())

	if !waitFor(t, 5*time.Second, func() bool { return s.Down() != "" }) {
		t.Fatal("nothing was reported when the engines were missing")
	}
	if !strings.Contains(s.Down(), "auto_install") {
		t.Errorf("Down() = %q, want it to name the setting", s.Down())
	}
}

// The installer's choices have to reach the server that starts next, or it
// would load weights that were never downloaded.
func TestSpeechSupervisorAdoptsWhatTheInstallerChose(t *testing.T) {
	s, _ := speechSupervisorFor(t, "serve", func(c *SpeechConfig) {
		c.WhisperModel = ""
		c.PiperVoice = ""
	})
	s.adopt(SpeechChoices{
		SttEngine:    "parakeet",
		SttModel:     "nemo-parakeet-tdt-0.6b-v3",
		WhisperModel: "small", WhisperDevice: "cuda", WhisperCompute: "float16",
		PiperVoice: "es_MX-ald-medium",
	})
	if s.cfg.WhisperModel != "small" || s.cfg.PiperVoice != "es_MX-ald-medium" {
		t.Errorf("cfg = %+v", s.cfg)
	}
	if s.cfg.WhisperDevice != "cuda" || s.cfg.WhisperCompute != "float16" {
		t.Errorf("hardware choices lost: %+v", s.cfg)
	}
	if s.cfg.SttEngine != "parakeet" || s.cfg.SttModel != "nemo-parakeet-tdt-0.6b-v3" {
		t.Errorf("engine choice lost: %+v", s.cfg)
	}

	// An empty choice must not erase a configured one.
	s.adopt(SpeechChoices{})
	if s.cfg.WhisperModel != "small" || s.cfg.PiperVoice != "es_MX-ald-medium" {
		t.Errorf("an empty result overwrote the configuration: %+v", s.cfg)
	}
	if s.cfg.SttEngine != "parakeet" || s.cfg.SttModel != "nemo-parakeet-tdt-0.6b-v3" {
		t.Errorf("an empty result erased the engine: %+v", s.cfg)
	}
}

// A settled Parakeet choice names no Whisper model at all, and must read as
// prepared — or every boot would re-run the installer.
func TestSpeechSupervisorSettledParakeetNeedsNoPrepare(t *testing.T) {
	home := t.TempDir()
	settled := newSpeechSupervisor(SpeechConfig{
		SttEngine: "parakeet", SttModel: "nemo-parakeet-tdt-0.6b-v3",
	}, home, "es", "tok", true, false)
	if settled.needsPrepare() {
		t.Error("a settled parakeet choice was re-prepared")
	}
	unchosen := newSpeechSupervisor(SpeechConfig{SttEngine: "parakeet"}, home, "es", "tok", true, false)
	if !unchosen.needsPrepare() {
		t.Error("an engine with no model chosen was treated as prepared")
	}
}

// The voice shell waits on this before it probes, so it has to report readiness
// promptly rather than after a fixed sleep.
func TestSpeechSupervisorWaitHealthy(t *testing.T) {
	s, _ := speechSupervisorFor(t, "serve", nil)
	s.start(context.Background())

	start := time.Now()
	if !s.waitHealthy(context.Background(), 10*time.Second) {
		t.Fatalf("waitHealthy gave up: %s", s.Down())
	}
	if time.Since(start) > 9*time.Second {
		t.Error("waitHealthy did not return as soon as the server was up")
	}
}

// A healthy server is polled for as long as it lives, so a process that dies
// quietly is noticed rather than assumed well.
func TestSpeechSupervisorKeepsPollingAHealthyServer(t *testing.T) {
	s, _ := speechSupervisorFor(t, "serve", nil)
	s.start(context.Background())
	if !waitFor(t, 10*time.Second, s.Healthy) {
		t.Fatalf("never became healthy: %s", s.Down())
	}
	// Outlive several probe intervals: the poll loop has to keep reporting
	// healthy rather than latching on the first success.
	time.Sleep(300 * time.Millisecond)
	if !s.Healthy() {
		t.Error("a server that is still up was reported unhealthy")
	}
}

// A server already listening — restarted gateway, or one the user started —
// is adopted rather than duplicated, and then watched for as long as it lives.
func TestSpeechSupervisorAdoptsAServerAlreadyListening(t *testing.T) {
	port := freePort(t)
	srv := &http.Server{
		Addr: fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}),
		ReadHeaderTimeout: time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	home := t.TempDir()
	// No interpreter and no install: if the supervisor tried to spawn, it
	// would fail loudly instead of finding what is already there.
	off := false
	s := newSpeechSupervisor(SpeechConfig{Port: port, AutoInstall: &off}, home, "en", "tok", true, true)
	s.probeInterval = 50 * time.Millisecond
	t.Cleanup(s.stop)
	s.start(context.Background())

	if !waitFor(t, 5*time.Second, s.Healthy) {
		t.Fatalf("a server already listening was not adopted: %s", s.Down())
	}
	// Outlive a few probes: the watch keeps running rather than latching.
	time.Sleep(200 * time.Millisecond)
	if !s.Healthy() || s.Down() != "" {
		t.Errorf("healthy=%v down=%q", s.Healthy(), s.Down())
	}

	// When it goes away, that is noticed.
	_ = srv.Close()
	_ = ln.Close()
	if !waitFor(t, 5*time.Second, func() bool { return !s.Healthy() }) {
		t.Error("a server that went away was still reported healthy")
	}
}

// The install runs from the supervisor too, so a gateway started before setup
// finished still ends up with a working local tier.
func TestSpeechSupervisorInstallsWhenTheEnginesAreMissing(t *testing.T) {
	home := t.TempDir()

	restorePath, restoreCmd, restoreEnv := lookPath, runCmd, runCmdEnv
	t.Cleanup(func() { lookPath, runCmd, runCmdEnv = restorePath, restoreCmd, restoreEnv })
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }

	var installed atomic.Bool
	runCmd = func(_ context.Context, argv []string) (string, error) {
		if len(argv) > 2 && argv[1] == "-m" && argv[2] == "venv" {
			python := speechVenvPython(home)
			if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
				return "", err
			}
			return "", os.WriteFile(python, []byte("#!/bin/sh\nexit 0\n"), 0o755)
		}
		if len(argv) > 1 && argv[1] == "install" {
			installed.Store(true)
			return "", nil
		}
		if len(argv) > 1 && argv[1] == "-c" {
			if strings.Contains(argv[2], "version_info") || installed.Load() {
				return "", nil
			}
			return "", errors.New("ModuleNotFoundError")
		}
		return "", nil
	}
	runCmdEnv = func(context.Context, []string, []string) (string, error) {
		return `{"whisper_model":"base","whisper_device":"cpu","whisper_compute":"int8","piper_voice":"en_US-lessac-medium"}`, nil
	}

	s := newSpeechSupervisor(SpeechConfig{Port: freePort(t)}, home, "en", "tok", true, true)
	s.probeInterval = 50 * time.Millisecond
	t.Cleanup(s.stop)

	if _, err := s.resolveCommand(context.Background()); err != nil {
		t.Fatalf("resolveCommand: %v", err)
	}
	if !installed.Load() {
		t.Error("the engines were never installed")
	}
	if s.cfg.PiperVoice != "en_US-lessac-medium" {
		t.Errorf("the installer's voice was not adopted: %+v", s.cfg)
	}
}

// A failed install must be attempted once, not on every restart: a machine
// that cannot install must not re-run a half-hour download in a loop.
func TestSpeechSupervisorInstallsAtMostOnce(t *testing.T) {
	restorePath, restoreCmd := lookPath, runCmd
	t.Cleanup(func() { lookPath, runCmd = restorePath, restoreCmd })
	lookPath = func(string) (string, error) { return "", errors.New("no python") }

	s := newSpeechSupervisor(SpeechConfig{Port: freePort(t)}, t.TempDir(), "en", "tok", true, true)
	if _, err := s.resolveCommand(context.Background()); err == nil {
		t.Fatal("an install with no interpreter should fail")
	}
	_, err := s.resolveCommand(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already failed") {
		t.Errorf("second attempt = %v, want it to refuse to retry", err)
	}
}

func TestSpeechDataDirIsConfigurable(t *testing.T) {
	if got := speechDataDir(SpeechConfig{DataDir: "/models"}, "/home"); got != "/models" {
		t.Errorf("data dir = %q, want the configured one", got)
	}
	if got := speechDataDir(SpeechConfig{}, "/home"); got != filepath.Join("/home", "speech") {
		t.Errorf("data dir = %q, want it under the Factor home", got)
	}
}

func TestSpeechSupervisorWaitHealthyGivesUp(t *testing.T) {
	home := t.TempDir()
	s := newSpeechSupervisor(SpeechConfig{Port: freePort(t)}, home, "en", "tok", true, true)
	s.probeInterval = 50 * time.Millisecond
	if s.waitHealthy(context.Background(), 200*time.Millisecond) {
		t.Error("waitHealthy reported a server that was never started as ready")
	}
}

// The exported façade is what the PC voice channel supervises the speech
// server through; it must drive the same lifecycle the phone does.
func TestSpeechServerFacadeSupervisesTheServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_TEST_SPEECH_MODE", "serve")
	cfg := SpeechConfig{
		Port:         freePort(t),
		Command:      os.Args[0],
		WhisperModel: "base",
		PiperVoice:   "en_US-lessac-medium",
	}
	plantVoice(t, home, cfg.PiperVoice)
	s := NewSpeechServer(cfg, home, "en", "boot-token", true, true)
	s.SetProbeInterval(50 * time.Millisecond)
	t.Cleanup(s.Stop)
	s.Start(context.Background())

	if !s.WaitHealthy(context.Background(), 10*time.Second) {
		t.Fatalf("the speech server never became healthy: %s", s.Down())
	}
	if !s.Healthy() || s.Down() != "" {
		t.Errorf("healthy=%v down=%q", s.Healthy(), s.Down())
	}
	if err := ProbeSpeechServer(context.Background(), SpeechBaseURL(cfg)); err != nil {
		t.Errorf("the probe could not reach the server the facade started: %v", err)
	}
}

func TestSpeechBaseURLNamesTheConfiguredPort(t *testing.T) {
	if got := SpeechBaseURL(SpeechConfig{Port: 9000}); got != "http://127.0.0.1:9000/v1" {
		t.Errorf("SpeechBaseURL = %q", got)
	}
}

// A cancelled context stops the wait immediately: the gateway is shutting down
// and nothing should sit on a three-minute deadline.
func TestSpeechSupervisorWaitHealthyStopsOnCancel(t *testing.T) {
	home := t.TempDir()
	s := newSpeechSupervisor(SpeechConfig{Port: freePort(t)}, home, "en", "tok", true, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if s.waitHealthy(ctx, time.Minute) {
		t.Error("a cancelled wait reported ready")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("a cancelled wait did not return promptly")
	}
}
