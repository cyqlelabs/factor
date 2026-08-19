package phone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A local tier with no server named is the case the wizard writes, and the one
// that has to work without the user doing anything: it resolves to the server
// Factor runs itself.
func TestLocalTierWithoutABaseURLIsManaged(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(*Config)
		wantSTTLocal  bool
		wantTTSLocal  bool
		wantTier      int
		wantSTTManage bool
		wantTTSManage bool
	}{
		{
			name:          "fully local audio",
			mutate:        func(c *Config) { c.STT.Provider = providerLocalOpenAI; c.TTS.Provider = providerLocalOpenAI },
			wantTier:      4,
			wantSTTManage: true,
			wantTTSManage: true,
		},
		{
			name:          "local transcription only",
			mutate:        func(c *Config) { c.STT.Provider = providerLocalOpenAI },
			wantTier:      2,
			wantSTTManage: true,
		},
		{
			name:          "local voice only",
			mutate:        func(c *Config) { c.TTS.Provider = providerLocalOpenAI },
			wantTier:      3,
			wantTTSManage: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := prepared(t, c.mutate)
			if err := cfg.validate(); err != nil {
				t.Fatalf("validate rejected a managed local tier: %v", err)
			}
			if !cfg.managedSpeech() {
				t.Fatal("a local tier with no base_url should be Factor's own server")
			}
			if got := cfg.Tier(); got != c.wantTier {
				t.Errorf("Tier() = %d, want %d", got, c.wantTier)
			}
			want := speechBaseURL(cfg.SpeechServer)
			if c.wantSTTManage && cfg.STT.BaseURL != want {
				t.Errorf("stt.base_url = %q, want the managed server %q", cfg.STT.BaseURL, want)
			}
			if c.wantTTSManage && cfg.TTS.BaseURL != want {
				t.Errorf("tts.base_url = %q, want the managed server %q", cfg.TTS.BaseURL, want)
			}
		})
	}
}

// A user who runs their own server keeps it: nothing is installed or
// supervised on their behalf, and their endpoint is left exactly as written.
func TestAnExplicitBaseURLIsNotManaged(t *testing.T) {
	cfg := prepared(t, func(c *Config) {
		c.STT.Provider = providerLocalOpenAI
		c.STT.BaseURL = "http://127.0.0.1:9999/v1/"
		c.TTS.Provider = providerLocalOpenAI
		c.TTS.BaseURL = "http://127.0.0.1:9999/v1"
	})
	if cfg.managedSpeech() {
		t.Fatal("a base_url the user set should not be treated as Factor's own server")
	}
	if cfg.STT.BaseURL != "http://127.0.0.1:9999/v1" {
		t.Errorf("stt.base_url = %q, want the trailing slash trimmed and nothing else", cfg.STT.BaseURL)
	}
}

// The managed server must never be handed the cloud keys kept for fallback,
// and a server the user runs must never be handed Factor's boot secret.
func TestOnlyTheManagedSpeechServerGetsTheBootSecret(t *testing.T) {
	managed := prepared(t, func(c *Config) {
		c.STT.Provider = providerLocalOpenAI
		c.TTS.Provider = providerLocalOpenAI
	})
	shell := renderShellConfig(managed, 8724, "boot-secret")
	if shell.STT.APIKey != "boot-secret" || shell.TTS.APIKey != "boot-secret" {
		t.Errorf("managed server got stt=%q tts=%q, want the boot secret in both",
			shell.STT.APIKey, shell.TTS.APIKey)
	}

	byo := prepared(t, func(c *Config) {
		c.STT.Provider = providerLocalOpenAI
		c.STT.BaseURL = "http://127.0.0.1:9999/v1"
		c.TTS.Provider = providerLocalOpenAI
		c.TTS.BaseURL = "http://127.0.0.1:9999/v1"
	})
	shell = renderShellConfig(byo, 8724, "boot-secret")
	if shell.STT.APIKey != "" || shell.TTS.APIKey != "" {
		t.Errorf("a server the user runs got stt=%q tts=%q, want no secret at all",
			shell.STT.APIKey, shell.TTS.APIKey)
	}
	for _, key := range []string{shell.STT.APIKey, shell.TTS.APIKey} {
		if key == managed.STTAPIKey || (key != "" && key == managed.ElevenLabsAPIKey) {
			t.Error("a local server was handed a cloud key kept for fallback")
		}
	}
}

// Patter's OpenAI adapter posts whatever model it is given; a blank one would
// put "model": null on the wire, which a stricter server rejects.
func TestLocalVoiceGetsAModelIdTheDialectAccepts(t *testing.T) {
	cfg := prepared(t, func(c *Config) { c.TTS.Provider = providerLocalOpenAI })
	if got := renderShellConfig(cfg, 8724, "t").TTS.Model; got != localTTSModel {
		t.Errorf("tts model = %q, want %q", got, localTTSModel)
	}
	cfg = prepared(t, func(c *Config) {
		c.TTS.Provider = providerLocalOpenAI
		c.TTS.Model = "kokoro"
	})
	if got := renderShellConfig(cfg, 8724, "t").TTS.Model; got != "kokoro" {
		t.Errorf("tts model = %q, want the configured one kept", got)
	}
}

// The speech server only loads the half of the pipeline the tier actually
// uses: a tier-2 machine must not pay for a voice it will never speak.
func TestSpeechServerOnlyLoadsTheHalvesTheTierUses(t *testing.T) {
	cfg := prepared(t, func(c *Config) { c.STT.Provider = providerLocalOpenAI })
	rendered := renderSpeechConfig(cfg.SpeechServer, "/home", cfg.Language, "tok",
		cfg.localSTT(), cfg.localTTS())
	if !rendered.NeedSTT || rendered.NeedTTS {
		t.Errorf("need_stt=%v need_tts=%v, want transcription only", rendered.NeedSTT, rendered.NeedTTS)
	}
	if rendered.Port != defaultSpeechPort {
		t.Errorf("port = %d, want %d", rendered.Port, defaultSpeechPort)
	}
	if rendered.DataDir != filepath.Join("/home", "speech") {
		t.Errorf("data_dir = %q, want it under the Factor home", rendered.DataDir)
	}
}

// The language reaches the server, because it is what picks the voice and
// tells the transcriber what it is listening to.
func TestSpeechConfigCarriesTheLanguage(t *testing.T) {
	cfg := prepared(t, func(c *Config) {
		c.Language = "es-MX"
		c.TTS.Provider = providerLocalOpenAI
	})
	rendered := renderSpeechConfig(cfg.SpeechServer, "/home", cfg.Language, "tok", false, true)
	if rendered.Language != "es-MX" {
		t.Errorf("language = %q, want it passed through untouched", rendered.Language)
	}
}

func TestSpeechChoicesSummary(t *testing.T) {
	cases := []struct {
		choices SpeechChoices
		want    string
	}{
		{SpeechChoices{WhisperModel: "small", WhisperDevice: "cpu", PiperVoice: "es_ES-davefx-medium"},
			"small · es_ES-davefx-medium"},
		{SpeechChoices{WhisperModel: "small", WhisperDevice: "cuda", PiperVoice: "en_US-lessac-medium"},
			"small on cuda · en_US-lessac-medium"},
		{SpeechChoices{WhisperModel: "base", WhisperDevice: "cpu"}, "base"},
	}
	for _, c := range cases {
		if got := c.choices.Summary(); got != c.want {
			t.Errorf("Summary() = %q, want %q", got, c.want)
		}
	}
}

// The installer's answer is the last JSON line; everything before it is
// progress the engines write to stderr as they download.
func TestParseSpeechChoices(t *testing.T) {
	out := strings.Join([]string{
		"[speechserver] resolving a voice language=es",
		"Downloading model.onnx: 100%|####| 63.2M/63.2M",
		`{"whisper_model":"small","whisper_device":"cuda","whisper_compute":"float16","piper_voice":"es_ES-davefx-medium"}`,
	}, "\n")
	choices, err := parseSpeechChoices(out)
	if err != nil {
		t.Fatalf("parseSpeechChoices: %v", err)
	}
	if choices.WhisperModel != "small" || choices.PiperVoice != "es_ES-davefx-medium" {
		t.Errorf("parsed %+v", choices)
	}
	if choices.WhisperDevice != "cuda" || choices.WhisperCompute != "float16" {
		t.Errorf("hardware choices lost: %+v", choices)
	}
}

func TestParseSpeechChoicesRejectsSilence(t *testing.T) {
	if _, err := parseSpeechChoices("Traceback (most recent call last):\nOSError: no space left"); err == nil {
		t.Fatal("an installer that printed no result should be an error")
	}
}

// A venv without the engines is not a usable interpreter: reporting it as
// ready would make the supervisor restart-loop against an ImportError.
func TestFindSpeechPythonRequiresTheEngines(t *testing.T) {
	home := t.TempDir()
	if _, ok := FindSpeechPython(home); ok {
		t.Fatal("an empty home should have no speech interpreter")
	}

	python := speechVenvPython(home)
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	restore := runCmd
	t.Cleanup(func() { runCmd = restore })
	runCmd = func(context.Context, []string) (string, error) {
		return "", errors.New("ModuleNotFoundError: faster_whisper")
	}
	if _, ok := FindSpeechPython(home); ok {
		t.Error("a venv missing the engines should not be reported as usable")
	}

	runCmd = func(context.Context, []string) (string, error) { return "", nil }
	if _, ok := FindSpeechPython(home); !ok {
		t.Error("a venv with the engines should be usable")
	}
}

// The pinned set is what the embedded server is written against, so the
// resampling wheel that 3.13 needs has to be in it.
func TestSpeechPackagesPinTheEngines(t *testing.T) {
	joined := strings.Join(speechPackages(), " ")
	for _, want := range []string{fasterWhisperSpec, piperSpec, "audioop-lts", "python_version"} {
		if !strings.Contains(joined, want) {
			t.Errorf("speechPackages() is missing %q: %v", want, speechPackages())
		}
	}
}

// The whole unattended install, from an empty home to a reported result: a
// virtualenv, the pinned engines, and the weights for the language.
func TestInstallSpeechBuildsTheVenvAndFetchesTheModels(t *testing.T) {
	home := t.TempDir()

	restorePath, restoreCmd, restoreEnv := lookPath, runCmd, runCmdEnv
	t.Cleanup(func() { lookPath, runCmd, runCmdEnv = restorePath, restoreCmd, restoreEnv })
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }

	var ran [][]string
	runCmd = func(_ context.Context, argv []string) (string, error) {
		ran = append(ran, argv)
		// The version probe that decides whether an interpreter is new enough,
		// and the one that reports whether the engines are importable.
		if len(argv) > 1 && argv[1] == "-c" {
			if strings.Contains(argv[2], "version_info") {
				return "", nil
			}
			// Not installed until pip has run.
			for _, prior := range ran {
				if len(prior) > 1 && prior[1] == "install" {
					return "", nil
				}
			}
			return "", errors.New("ModuleNotFoundError")
		}
		if len(argv) > 2 && argv[1] == "-m" && argv[2] == "venv" {
			// Materialise the interpreter the installer will look for next.
			python := speechVenvPython(home)
			if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
				return "", err
			}
			return "", os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755)
		}
		return "", nil
	}
	runCmdEnv = func(_ context.Context, argv []string, _ []string) (string, error) {
		ran = append(ran, argv)
		return `{"whisper_model":"tiny","whisper_device":"cpu","whisper_compute":"int8","piper_voice":"sw_CD-lanfrica-medium"}`, nil
	}

	var steps []string
	choices, err := InstallSpeech(context.Background(), home, "sw", SpeechConfig{}, true, true,
		func(format string, args ...any) { steps = append(steps, fmt.Sprintf(format, args...)) })
	if err != nil {
		t.Fatalf("InstallSpeech: %v", err)
	}
	if choices.PiperVoice != "sw_CD-lanfrica-medium" || choices.WhisperModel != "tiny" {
		t.Errorf("choices = %+v", choices)
	}

	var sawVenv, sawPip, sawPrepare bool
	for _, argv := range ran {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "-m venv"):
			sawVenv = true
		case strings.Contains(joined, "install"):
			sawPip = true
			for _, want := range []string{fasterWhisperSpec, piperSpec} {
				if !strings.Contains(joined, want) {
					t.Errorf("pip was not asked for %q: %s", want, joined)
				}
			}
		case strings.Contains(joined, "--prepare"):
			sawPrepare = true
		}
	}
	if !sawVenv || !sawPip || !sawPrepare {
		t.Errorf("venv=%v pip=%v prepare=%v — the install skipped a step", sawVenv, sawPip, sawPrepare)
	}
	if len(steps) == 0 {
		t.Error("the install reported no progress at all")
	}
}

// A machine with no usable Python cannot be fixed by downloading more, so it
// has to say so rather than half-building a virtualenv.
func TestInstallSpeechWithoutAPython(t *testing.T) {
	restore := lookPath
	t.Cleanup(func() { lookPath = restore })
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	_, err := InstallSpeech(context.Background(), t.TempDir(), "en", SpeechConfig{}, true, true, nil)
	if err == nil {
		t.Fatal("an install with no interpreter should fail")
	}
	if !strings.Contains(err.Error(), "Python") {
		t.Errorf("error = %q, want it to name the missing thing", err)
	}
}

// The installer takes its configuration through the environment, like every
// other child Factor spawns, so no secret is visible in argv.
func TestPrepareSpeechPassesConfigThroughTheEnvironment(t *testing.T) {
	home := t.TempDir()
	python := speechVenvPython(home)
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	restoreCmd, restoreEnv := runCmd, runCmdEnv
	t.Cleanup(func() { runCmd, runCmdEnv = restoreCmd, restoreEnv })
	runCmd = func(context.Context, []string) (string, error) { return "", nil }

	var sawArgv []string
	var sawConfig speechServerConfig
	runCmdEnv = func(_ context.Context, argv []string, env []string) (string, error) {
		sawArgv = argv
		for _, entry := range env {
			if blob, ok := strings.CutPrefix(entry, "FACTOR_SPEECH_CONFIG="); ok {
				if err := json.Unmarshal([]byte(blob), &sawConfig); err != nil {
					return "", err
				}
			}
		}
		return fmt.Sprintf(`{"whisper_model":"base","whisper_device":"cpu","whisper_compute":"int8","piper_voice":%q}`,
			"es_ES-davefx-medium"), nil
	}

	choices, err := PrepareSpeech(context.Background(), home, "es", SpeechConfig{}, true, true, nil)
	if err != nil {
		t.Fatalf("PrepareSpeech: %v", err)
	}
	if choices.PiperVoice != "es_ES-davefx-medium" {
		t.Errorf("voice = %q", choices.PiperVoice)
	}
	if sawConfig.Language != "es" || !sawConfig.NeedTTS {
		t.Errorf("the installer was not told what to fetch: %+v", sawConfig)
	}
	if len(sawArgv) < 3 || sawArgv[2] != "--prepare" {
		t.Errorf("argv = %v, want the script run with --prepare", sawArgv)
	}
	for _, arg := range sawArgv {
		if strings.Contains(arg, "es_ES") || strings.Contains(arg, "{") {
			t.Errorf("configuration leaked into argv: %v", sawArgv)
		}
	}
	if _, err := os.Stat(speechScriptPath(home)); err != nil {
		t.Errorf("the speech server script was not written: %v", err)
	}
}

// The local speech tier is chosen by people who do not want their machine
// talking to anyone. onnxruntime, which the speech engines run on, ships
// Microsoft's telemetry client and queues device data while it registers its
// execution providers — during the import, before any model is loaded. The
// switch therefore has to be thrown before the first import that pulls
// onnxruntime in, and that is faster-whisper's VAD, not Piper. Getting this
// order wrong still silences Piper and leaks everything anyway.
func TestEmbeddedSpeechServerSilencesOnnxTelemetryBeforeAnyImport(t *testing.T) {
	script := string(speechServerScript)
	if !strings.Contains(script, "disable_telemetry_events") {
		t.Fatal("the script never calls onnxruntime's own switch")
	}
	if !strings.Contains(script, "def mute_onnx_telemetry") {
		t.Fatal("the helper is gone")
	}
	// Every import that reaches onnxruntime must be preceded by a call. The
	// definition also matches the bare name, so the call is matched with its
	// indentation.
	for _, importer := range []string{
		"from faster_whisper import WhisperModel",
		"from piper import PiperVoice",
	} {
		for _, at := range indexesOf(script, importer) {
			if !mutedBefore(script[:at]) {
				t.Errorf("%q at offset %d is reached without disabling telemetry first", importer, at)
			}
		}
	}
}

// mutedBefore reports whether the script disables telemetry somewhere in the
// text leading up to an import.
func mutedBefore(prefix string) bool {
	return strings.Contains(prefix, "\n    mute_onnx_telemetry()\n") ||
		strings.Contains(prefix, "\n        mute_onnx_telemetry()\n")
}

func indexesOf(haystack, needle string) []int {
	var out []int
	for at := 0; ; {
		i := strings.Index(haystack[at:], needle)
		if i < 0 {
			return out
		}
		out = append(out, at+i)
		at += i + len(needle)
	}
}
