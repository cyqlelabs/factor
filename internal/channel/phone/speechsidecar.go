package phone

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The speech server is supervised the way the voice shell and the memory
// engine are — spawn, health-poll, restart with backoff — and its absence is
// never fatal on its own: with local_audio_fallback on, a speech server that
// will not come up degrades the call to the cloud tier instead of killing it.

type speechSupervisor struct {
	cfg         SpeechConfig
	home        string
	language    string
	token       string
	needSTT     bool
	needTTS     bool
	needSpeaker bool

	scriptPath string
	client     *controlClient

	healthy      atomic.Bool
	down         atomic.Value // string: why it cannot run, "" when fine
	installTried atomic.Bool
	cancel       context.CancelFunc
	wg           sync.WaitGroup

	probeInterval time.Duration
}

func newSpeechSupervisor(cfg SpeechConfig, home, language, token string, needSTT, needTTS, needSpeaker bool) *speechSupervisor {
	client := newControlClient(fmt.Sprintf("http://127.0.0.1:%d", speechPort(cfg)))
	client.token = token
	return &speechSupervisor{
		cfg:         cfg,
		home:        home,
		language:    language,
		token:       token,
		needSTT:     needSTT,
		needTTS:     needTTS,
		needSpeaker: needSpeaker,
		scriptPath:  speechScriptPath(home),
		client:      client,
	}
}

// speechEnv is the environment the speech process is born with.
//
// onnxruntime, which both speech engines run on, posts the machine to
// Microsoft the moment it initializes — OS build, CPU model, memory, network
// type, a persistent device id, and the interpreter path, which carries the
// user's name. Its own disable_telemetry_events() cannot stop that: the
// events are logged while the environment is created, before any call can be
// made against it (microsoft/onnxruntime#25573). Only ORT_DISABLE_TELEMETRY,
// read before initialization, prevents the uploader and the device id from
// existing at all, which is why it is set out here rather than in the script.
func speechEnv() []string {
	return append(os.Environ(), "ORT_DISABLE_TELEMETRY=1")
}

func (s *speechSupervisor) Healthy() bool { return s.healthy.Load() }

func (s *speechSupervisor) Down() string {
	reason, _ := s.down.Load().(string)
	return reason
}

func (s *speechSupervisor) setDown(format string, args ...any) {
	s.down.Store(fmt.Sprintf(format, args...))
}

func (s *speechSupervisor) reprobeInterval() time.Duration {
	if s.probeInterval > 0 {
		return s.probeInterval
	}
	return 15 * time.Second
}

func (s *speechSupervisor) start(parent context.Context) {
	s.down.Store("")
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go s.run(ctx)
}

func (s *speechSupervisor) stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *speechSupervisor) run(ctx context.Context) {
	defer s.wg.Done()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		if s.client.health(ctx) == nil {
			s.healthy.Store(true)
			s.down.Store("")
			backoff = 5 * time.Second
			s.pollWhileHealthy(ctx)
			continue
		}
		s.healthy.Store(false)
		err := s.spawnAndWait(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("speech server exited; restarting", "error", err, "backoff", backoff)
		sleepCtx(ctx, backoff)
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

func (s *speechSupervisor) pollWhileHealthy(ctx context.Context) {
	for ctx.Err() == nil {
		sleepCtx(ctx, s.reprobeInterval())
		if ctx.Err() != nil {
			return
		}
		if s.client.health(ctx) != nil {
			s.healthy.Store(false)
			return
		}
	}
}

// resolveCommand locates the interpreter, installing the engines and the
// weights when they are missing. As with Patter, the install is attempted at
// most once per process: a machine that cannot install must not re-run a
// half-hour download on every restart.
func (s *speechSupervisor) resolveCommand(ctx context.Context) (string, error) {
	if s.cfg.Command != "" {
		return resolveInterpreter(s.cfg.Command)
	}
	if path, ok := FindSpeechPython(s.home); ok {
		if err := s.ensureSpeakerEngine(ctx, path); err != nil {
			return "", err
		}
		return path, nil
	}
	if !s.cfg.autoInstall() {
		return "", fmt.Errorf("the local speech engines are not installed and channels.phone.speech_server.auto_install is off")
	}
	if s.installTried.Swap(true) {
		return "", fmt.Errorf("the local speech engines are not installed and the automatic install already failed this run")
	}
	slog.Info("local speech engines missing; installing them automatically", "language", s.language)
	choices, err := InstallSpeech(ctx, s.home, s.language, s.cfg, s.needSTT, s.needTTS, s.needSpeaker,
		func(format string, args ...any) {
			slog.Info("speech install: " + fmt.Sprintf(format, args...))
		})
	if err != nil {
		return "", err
	}
	s.adopt(choices)
	slog.Info("local speech ready", "stack", choices.Summary())
	path, ok := FindSpeechPython(s.home)
	if !ok {
		return "", fmt.Errorf("the speech engines installed but %s is still not usable", SpeechVenvDir(s.home))
	}
	return path, nil
}

// adopt records what the installer chose, so the server that starts next runs
// the weights that were actually downloaded.
func (s *speechSupervisor) adopt(choices SpeechChoices) {
	if choices.SttEngine != "" {
		s.cfg.SttEngine = choices.SttEngine
	}
	if choices.SttModel != "" {
		s.cfg.SttModel = choices.SttModel
	}
	if choices.WhisperModel != "" {
		s.cfg.WhisperModel = choices.WhisperModel
	}
	if choices.WhisperDevice != "" {
		s.cfg.WhisperDevice = choices.WhisperDevice
	}
	if choices.WhisperCompute != "" {
		s.cfg.WhisperCompute = choices.WhisperCompute
	}
	if choices.PiperVoice != "" {
		s.cfg.PiperVoice = choices.PiperVoice
	}
}

// ensureSpeakerEngine backfills sherpa-onnx into an already-built virtualenv
// when speaker identification was turned on after the engines were installed.
func (s *speechSupervisor) ensureSpeakerEngine(ctx context.Context, python string) error {
	if !s.needSpeaker || hasSpeakerEngine(python) {
		return nil
	}
	if !s.cfg.autoInstall() {
		return fmt.Errorf("speaker identification needs %s and auto_install is off", sherpaOnnxSpec)
	}
	if s.installTried.Swap(true) {
		return fmt.Errorf("the speaker engine is not installed and the automatic install already failed this run")
	}
	slog.Info("installing the speaker-identification engine", "spec", sherpaOnnxSpec)
	ctx, cancel := context.WithTimeout(ctx, SpeechInstallTimeout)
	defer cancel()
	if out, err := runCmd(ctx, []string{speechVenvPip(s.home), "install", sherpaOnnxSpec}); err != nil {
		return fmt.Errorf("could not install %s: %v\n%s", sherpaOnnxSpec, err, lastLines(out, 8))
	}
	return nil
}

// needsPrepare reports whether the weights the server would load are settled:
// unchosen halves need choosing, and a voice named in the config — by the
// wizard's picker or a hand edit — needs its files actually on disk, or the
// server would crash-loop against a voice it cannot load.
func (s *speechSupervisor) needsPrepare() bool {
	if s.needSTT && s.cfg.WhisperModel == "" && s.cfg.SttModel == "" {
		return true
	}
	if s.needSpeaker {
		// Both halves: the embedding model names a voice, the segmentation
		// model says how many voices there were. Without the second one two
		// people in one recording answer as one person, which is a wrong
		// answer rather than a missing feature.
		for _, path := range []string{
			speakerModelPath(s.cfg, s.home), segmentationModelPath(s.cfg, s.home),
		} {
			if _, err := os.Stat(path); err != nil {
				return true
			}
		}
	}
	if !s.needTTS {
		return false
	}
	if s.cfg.PiperVoice == "" {
		return true
	}
	_, err := os.Stat(filepath.Join(speechDataDir(s.cfg, s.home), "piper", s.cfg.PiperVoice+".onnx"))
	return err != nil
}

func (s *speechSupervisor) spawnAndWait(ctx context.Context) error {
	command, err := s.resolveCommand(ctx)
	if err != nil {
		s.setDown("%v", err)
		return err
	}
	// The weights can be missing even when the engines are not — a language
	// changed in the config, say — so prepare runs whenever the server has not
	// been told what to load. It settles the hardware question too, which is
	// why a transcription-only tier needs it just as much as a voice does.
	if s.needsPrepare() {
		choices, prepErr := PrepareSpeech(ctx, s.home, s.language, s.cfg, s.needSTT, s.needTTS, s.needSpeaker,
			func(format string, args ...any) {
				slog.Info("speech models: " + fmt.Sprintf(format, args...))
			})
		if prepErr != nil {
			s.setDown("%v", prepErr)
			return prepErr
		}
		s.adopt(choices)
	}
	if err := WriteSpeechScript(s.scriptPath); err != nil {
		s.setDown("%v", err)
		return err
	}
	blob, err := json.Marshal(renderSpeechConfig(s.cfg, s.home, s.language, s.token, s.needSTT, s.needTTS, s.needSpeaker))
	if err != nil {
		return err
	}
	s.down.Store("")

	cmd := exec.CommandContext(ctx, command, s.scriptPath)
	cmd.Env = append(speechEnv(), "FACTOR_SPEECH_CONFIG="+string(blob))
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	logDir := filepath.Join(s.home, "logs")
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		if f, ferr := os.OpenFile(filepath.Join(logDir, "speechserver.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
			defer f.Close()
			cmd.Stdout, cmd.Stderr = f, f
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	stt := s.cfg.WhisperModel
	if s.cfg.SttEngine == "parakeet" {
		stt = s.cfg.SttModel
	}
	slog.Info("speech server started",
		"pid", cmd.Process.Pid, "port", speechPort(s.cfg), "stt", stt, "tts", s.cfg.PiperVoice)

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

func (s *speechSupervisor) pollUntilHealthy(ctx context.Context) {
	for ctx.Err() == nil {
		was := s.healthy.Load()
		if s.client.health(ctx) == nil {
			if !was {
				slog.Info("speech server healthy", "port", speechPort(s.cfg))
			}
			s.healthy.Store(true)
			sleepCtx(ctx, s.reprobeInterval())
			continue
		}
		s.healthy.Store(false)
		sleepCtx(ctx, 2*time.Second)
	}
}

// waitHealthy blocks until the speech server answers, or the deadline passes.
// The voice shell waits on this before it starts: the models take a while to
// load, and a shell that probed too early would decide the local tier was
// unreachable and quietly fall back to the cloud.
func (s *speechSupervisor) waitHealthy(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if s.healthy.Load() {
			return true
		}
		sleepCtx(ctx, 500*time.Millisecond)
	}
	return s.healthy.Load()
}
