package phone

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The local voice tiers need an OpenAI-compatible speech server, and there is
// no such thing to install: Speaches ships as a container or a git checkout,
// and Piper's own server speaks its own dialect. So Factor runs its own, in a
// private virtualenv beside the voice shell's, and — because a user who picks
// a local tier has asked for local speech, not for homework — installs the
// engines and downloads the weights itself.

//go:embed speechserver.py
var speechServerScript []byte

const (
	// speechPort is where the managed server listens. It sits clear of the
	// voice shell's control port, the webhook port next to it, and the bridge.
	defaultSpeechPort = 8726

	// Pinned because the embedded server is written against these APIs. Bump
	// them together with speechserver.py.
	fasterWhisperSpec = "faster-whisper==1.2.1"
	piperSpec         = "piper-tts==1.6.1"

	// SpeechInstallTimeout bounds one install. The wheels run to a few hundred
	// megabytes and the model weights follow them, on whatever connection the
	// user has.
	SpeechInstallTimeout = 30 * time.Minute

	// speechPrepareTimeout bounds the weight download alone.
	speechPrepareTimeout = 20 * time.Minute

	// speechReadyTimeout is how long the voice shell waits for the speech
	// server to finish loading its models. It covers a load, not a cold
	// install — the wizard does that — so a first boot that has to install
	// anyway spends this session on the cloud tier and picks local up after.
	speechReadyTimeout = 3 * time.Minute
)

// speechPackages is what the local speech server runs on. audioop carries the
// resampling and was removed from the standard library in 3.13, so it comes
// back as a wheel there — the same marker getpatter itself declares.
func speechPackages() []string {
	return []string{
		fasterWhisperSpec,
		piperSpec,
		"fastapi>=0.115.0",
		"uvicorn[standard]>=0.30.0",
		"python-multipart>=0.0.20",
		`audioop-lts>=0.2.1; python_version >= "3.13"`,
	}
}

// SpeechConfig is channels.phone.speech_server: the knobs on the speech server
// Factor manages. Every one is optional — the installer picks defaults that
// suit the machine and the language, and writes them back here.
type SpeechConfig struct {
	Port int `json:"port,omitempty"`

	// WhisperModel is a faster-whisper size ("tiny", "base", "small", …) or a
	// Hugging Face repo. Blank lets the installer choose for this machine.
	WhisperModel   string `json:"whisper_model,omitempty"`
	WhisperDevice  string `json:"whisper_device,omitempty"`
	WhisperCompute string `json:"whisper_compute,omitempty"`

	// PiperVoice names the voice, e.g. "es_MX-ald-medium". Blank resolves one
	// from the language against Piper's catalogue.
	PiperVoice string `json:"piper_voice,omitempty"`

	// DataDir holds the downloaded weights. Blank means ~/.factor/speech.
	DataDir string `json:"data_dir,omitempty"`

	Command     string `json:"command,omitempty"`
	AutoInstall *bool  `json:"auto_install,omitempty"`
}

func (s SpeechConfig) autoInstall() bool { return s.AutoInstall == nil || *s.AutoInstall }

// SpeechVenvDir is the private virtualenv the speech server runs in. It is
// deliberately not the voice shell's: the speech engines drag in a large,
// version-sensitive native stack, and a resolver conflict there must not be
// able to take the phone down with it.
func SpeechVenvDir(home string) string { return filepath.Join(home, "speech-venv") }

func speechVenvPython(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(SpeechVenvDir(home), "Scripts", "python.exe")
	}
	return filepath.Join(SpeechVenvDir(home), "bin", "python")
}

func speechVenvPip(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(SpeechVenvDir(home), "Scripts", "pip.exe")
	}
	return filepath.Join(SpeechVenvDir(home), "bin", "pip")
}

// speechDataDir is where the weights live, so a reinstall of the virtualenv
// does not re-download gigabytes.
func speechDataDir(cfg SpeechConfig, home string) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}
	return filepath.Join(home, "speech")
}

// FindSpeechPython returns the interpreter that can run the speech server —
// the private venv, once both engines are actually installed in it. A venv
// that exists but cannot import them is not usable: reporting it as ready
// would make the supervisor restart-loop against an ImportError.
func FindSpeechPython(home string) (string, bool) {
	python := speechVenvPython(home)
	if info, err := os.Stat(python); err != nil || info.IsDir() {
		return "", false
	}
	if !hasSpeechEngines(python) {
		return "", false
	}
	return python, true
}

func hasSpeechEngines(python string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := runCmd(ctx, []string{python, "-c",
		"import importlib.metadata as m; m.version('faster-whisper'); m.version('piper-tts')"})
	return err == nil
}

// SpeechChoices is what the installer settled on, which Factor writes back
// into the config so a call never has to rediscover it.
type SpeechChoices struct {
	WhisperModel   string `json:"whisper_model"`
	WhisperDevice  string `json:"whisper_device"`
	WhisperCompute string `json:"whisper_compute"`
	PiperVoice     string `json:"piper_voice"`

	// Warning is what the installer wants the user to know about the stack it
	// just built — a machine with no GPU gets a slower, less accurate
	// transcriber, and finding that out on a call is worse than being told.
	Warning string `json:"warning,omitempty"`
}

// InstallSpeech builds the virtualenv, installs the engines, and downloads the
// weights for a language — everything a local tier needs before it can take a
// call. needSTT and needTTS follow the tier, so a user who only wanted local
// transcription does not wait on a voice download.
func InstallSpeech(ctx context.Context, home, language string, cfg SpeechConfig,
	needSTT, needTTS bool, progress Progress) (SpeechChoices, error) {

	ctx, cancel := context.WithTimeout(ctx, SpeechInstallTimeout)
	defer cancel()

	python, err := systemPython()
	if err != nil {
		return SpeechChoices{}, err
	}
	if _, statErr := os.Stat(speechVenvPython(home)); statErr != nil {
		progress.emit("creating the speech virtualenv at %s…", SpeechVenvDir(home))
		if out, err := runCmd(ctx, []string{python, "-m", "venv", SpeechVenvDir(home)}); err != nil {
			return SpeechChoices{}, fmt.Errorf("could not create %s: %v\n%s",
				SpeechVenvDir(home), err, lastLines(out, 8))
		}
	}

	progress.emit("installing the speech engines (a few hundred megabytes)…")
	args := append([]string{speechVenvPip(home), "install", "--upgrade"}, speechPackages()...)
	if out, err := runCmd(ctx, args); err != nil {
		return SpeechChoices{}, fmt.Errorf("could not install the speech engines: %v\n%s",
			err, lastLines(out, 12))
	}

	return PrepareSpeech(ctx, home, language, cfg, needSTT, needTTS, progress)
}

// PrepareSpeech downloads the weights and reports what the installer chose for
// this machine and language. It is separate from the virtualenv build so a
// language change re-downloads a voice without reinstalling everything.
func PrepareSpeech(ctx context.Context, home, language string, cfg SpeechConfig,
	needSTT, needTTS bool, progress Progress) (SpeechChoices, error) {

	python, ok := FindSpeechPython(home)
	if !ok {
		return SpeechChoices{}, fmt.Errorf("the speech engines are not installed in %s", SpeechVenvDir(home))
	}
	script := speechScriptPath(home)
	if err := WriteSpeechScript(script); err != nil {
		return SpeechChoices{}, err
	}

	blob, err := json.Marshal(renderSpeechConfig(cfg, home, language, "", needSTT, needTTS))
	if err != nil {
		return SpeechChoices{}, err
	}

	progress.emit("downloading the speech models for %s…", language)
	ctx, cancel := context.WithTimeout(ctx, speechPrepareTimeout)
	defer cancel()
	out, err := runCmdEnv(ctx, []string{python, script, "--prepare"},
		append(os.Environ(), "FACTOR_SPEECH_CONFIG="+string(blob)))
	if err != nil {
		return SpeechChoices{}, fmt.Errorf("could not prepare the speech models: %v\n%s",
			err, lastLines(out, 12))
	}

	choices, err := parseSpeechChoices(out)
	if err != nil {
		return SpeechChoices{}, err
	}
	progress.emit("speech ready (%s)", choices.Summary())
	return choices, nil
}

// parseSpeechChoices reads the installer's answer, which is the last line of
// its output: everything before it is progress written to stderr.
func parseSpeechChoices(out string) (SpeechChoices, error) {
	var choices SpeechChoices
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &choices); err == nil && choices.WhisperModel != "" {
			return choices, nil
		}
	}
	return SpeechChoices{}, fmt.Errorf("the speech installer reported no result\n%s", lastLines(out, 12))
}

// Summary describes the installed stack in one line, for the wizard and status.
func (c SpeechChoices) Summary() string {
	stt := c.WhisperModel
	if c.WhisperDevice != "" && c.WhisperDevice != "cpu" {
		stt += " on " + c.WhisperDevice
	}
	if c.PiperVoice == "" {
		return stt
	}
	return stt + " · " + c.PiperVoice
}

// speechScriptPath is where the embedded server lands, next to the voice shell
// so a misbehaving call has both scripts to read.
func speechScriptPath(home string) string { return filepath.Join(home, "speechserver.py") }

// WriteSpeechScript materializes the embedded speech server, refreshing it
// whenever Factor is upgraded.
func WriteSpeechScript(path string) error {
	return writeScript(path, speechServerScript)
}

// speechServerConfig is the JSON the server reads out of its environment.
type speechServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`

	Token    string `json:"token,omitempty"`
	DataDir  string `json:"data_dir"`
	Language string `json:"language"`

	WhisperModel   string `json:"whisper_model,omitempty"`
	WhisperDevice  string `json:"whisper_device,omitempty"`
	WhisperCompute string `json:"whisper_compute,omitempty"`
	PiperVoice     string `json:"piper_voice,omitempty"`

	// NeedSTT and NeedTTS follow the tier: a tier that keeps one half in the
	// cloud must not pay for the other half's weights or memory.
	NeedSTT bool `json:"need_stt"`
	NeedTTS bool `json:"need_tts"`
}

func renderSpeechConfig(cfg SpeechConfig, home, language, token string, needSTT, needTTS bool) speechServerConfig {
	return speechServerConfig{
		Host:           "127.0.0.1",
		Port:           speechPort(cfg),
		Token:          token,
		DataDir:        speechDataDir(cfg, home),
		Language:       language,
		WhisperModel:   cfg.WhisperModel,
		WhisperDevice:  cfg.WhisperDevice,
		WhisperCompute: cfg.WhisperCompute,
		PiperVoice:     cfg.PiperVoice,
		NeedSTT:        needSTT,
		NeedTTS:        needTTS,
	}
}

func speechPort(cfg SpeechConfig) int {
	if cfg.Port > 0 {
		return cfg.Port
	}
	return defaultSpeechPort
}

// speechBaseURL is the endpoint the managed server answers on — what an empty
// stt.base_url or tts.base_url resolves to.
func speechBaseURL(cfg SpeechConfig) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", speechPort(cfg))
}
