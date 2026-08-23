// Package voice is the PC voice connector: the user talks to Factor through
// the machine's own microphone and hears it through the speakers. The speech
// tiers are the phone channel's — cloud STT/TTS or the managed local speech
// server — but the audio path is local: helper programs capture and play PCM,
// Go does the voice-activity detection, and each utterance runs synchronously
// through the agent loop so talking over a reply cancels it. Config section:
//
//	"channels": {"voice": {"activation": "wake-word", "wake_word": "factor", ...}}
//
// Everything is optional: no channels.voice section, nothing runs.
package voice

import (
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/channel/phone"
)

// What happens to a voice no enrolled profile matches.
const (
	unknownAnonymous = "anonymous"
	unknownEnroll    = "enroll"

	// defaultSpeakerThreshold sits in the gap the embedding model actually
	// leaves. Measured over real utterances from one microphone: the same
	// person scores 0.49 to 0.68 against themselves, and different people
	// -0.10 to 0.09. Anything at 0.5 clips the bottom of the same-speaker
	// range — which is how a person gets recognized on one sentence and
	// enrolled as a stranger on the next — while a bar this side of the gap
	// still leaves four times the headroom over an impostor.
	defaultSpeakerThreshold = 0.35
)

const (
	// defaultControlPort is the loopback endpoint `factor talk` reaches; the
	// default speech port sits clear of every port the phone channel uses
	// (8722, 8723, 8724, 8726), so both channels can run local speech at once.
	defaultControlPort = 8730
	defaultSpeechPort  = 8728

	defaultLanguage = "en"
	defaultWakeWord = "factor"

	// defaultFollowUpSeconds is how long after an exchange the wake word is
	// not required, so a conversation does not need it repeated per sentence.
	defaultFollowUpSeconds = 8
)

// Activation modes: who decides that an utterance was meant for the agent.
const (
	activationAlways   = "always"
	activationWakeWord = "wake-word"
	activationPTT      = "push-to-talk"
)

// Audio provider identifiers, spelled the way the phone channel spells them.
const (
	providerDeepgram    = "deepgram"
	providerWhisper     = "whisper"
	providerElevenLabs  = "elevenlabs"
	providerLocalOpenAI = "local-openai"
)

// Config is the channels.voice section. Every secret is a top-level string so
// the config layer's (non-recursive) secret scrubber redacts it from tool
// output for free — do not nest them.
type Config struct {
	Enabled *bool `json:"enabled,omitempty"`

	Language string `json:"language"`

	// Activation is "always" (every utterance is a turn), "wake-word" (only
	// utterances opening with WakeWord, plus a follow-up window after each
	// exchange), or "push-to-talk" (only after `factor talk` arms the mic).
	// The push-to-talk trigger works in every mode, as the rescue for a wake
	// word that misfires.
	Activation string `json:"activation"`
	WakeWord   string `json:"wake_word,omitempty"`
	// FollowUpSeconds is the wake-word grace window after a spoken reply.
	FollowUpSeconds int `json:"follow_up_seconds,omitempty"`

	STT       phone.AudioEndpoint `json:"stt"`
	STTAPIKey string              `json:"stt_api_key"`

	TTS              phone.AudioEndpoint `json:"tts"`
	ElevenLabsAPIKey string              `json:"elevenlabs_api_key"`
	VoiceID          string              `json:"voice_id,omitempty"`

	// SpeechServer tunes the local speech server Factor runs itself — the
	// same managed server the phone channel uses, on its own port.
	SpeechServer phone.SpeechConfig `json:"speech_server,omitempty"`

	// LocalAudioFallback falls back to the cloud tier when a configured local
	// speech server is unreachable at startup. nil means enabled.
	LocalAudioFallback *bool `json:"local_audio_fallback,omitempty"`

	// ControlPort is the loopback endpoint that arms push-to-talk.
	ControlPort int `json:"control_port"`

	// InputDevice and OutputDevice name the devices the capture and playback
	// helpers should use; blank means each helper's default.
	InputDevice  string `json:"input_device,omitempty"`
	OutputDevice string `json:"output_device,omitempty"`

	// SpeakerID turns on speaker identification: each utterance's voice is
	// matched against the enrolled profiles, so the people of one household
	// hold separate conversations. It needs the managed local speech server
	// (a local stt or tts tier with no base_url), which computes the voice
	// embeddings.
	SpeakerID bool `json:"speaker_id,omitempty"`
	// SpeakerThreshold is the cosine similarity below which a voice is not
	// anyone enrolled. 0 means the default.
	SpeakerThreshold float64 `json:"speaker_threshold,omitempty"`
	// UnknownSpeaker decides what happens to a voice no profile matches:
	// "enroll" creates a profile for it on the spot, "anonymous" (the
	// default) treats the utterance as the primary conversation, unnamed.
	UnknownSpeaker string `json:"unknown_speaker,omitempty"`

	// RoomIsolation scopes each turn to who can hear the reply rather than
	// to who is speaking: with company in the room the conversation runs in
	// its own session and reads its own memory space, so nothing said alone
	// is recalled out loud to a guest. It needs speaker_id, which is what
	// tells one voice from another. nil means enabled wherever speaker_id is.
	RoomIsolation *bool `json:"room_isolation,omitempty"`
	// RoomTimeoutMinutes is how long a voice counts as still in the room
	// after its last word. 0 means the default. Long on purpose: it measures
	// a visit, not a pause between sentences.
	RoomTimeoutMinutes int `json:"room_timeout_minutes,omitempty"`

	// OutputVolume scales the synthesized voice before it reaches the
	// speakers, as a percentage of full volume (1–100). Lowering it is the
	// blunt lever against the speakers feeding back into the microphone;
	// 0 means full volume.
	OutputVolume int `json:"output_volume,omitempty"`

	// VADRatio is how far above the noise floor speech has to rise;
	// BargeRatio is the same bar while the agent is speaking, set higher so
	// the speakers' own sound does not interrupt the reply.
	VADRatio   float64 `json:"vad_ratio,omitempty"`
	BargeRatio float64 `json:"barge_ratio,omitempty"`
	// SilenceMs is how much silence ends an utterance.
	SilenceMs int `json:"silence_ms,omitempty"`

	// Test overrides, following the Telegram precedent: the cloud speech
	// APIs' base URLs.
	STTAPIBase string `json:"stt_api_base,omitempty"`
	TTSAPIBase string `json:"tts_api_base,omitempty"`
}

func (c Config) localAudioFallback() bool {
	return c.LocalAudioFallback == nil || *c.LocalAudioFallback
}

// applyDefaults fills every unset knob, so an empty section is a complete
// cloud-tier configuration waiting only for its keys.
func (c *Config) applyDefaults() {
	if c.Language == "" {
		c.Language = defaultLanguage
	}
	c.Activation = strings.ToLower(strings.TrimSpace(c.Activation))
	if c.Activation == "" {
		c.Activation = activationAlways
	}
	c.WakeWord = strings.TrimSpace(c.WakeWord)
	if c.Activation == activationWakeWord && c.WakeWord == "" {
		c.WakeWord = defaultWakeWord
	}
	if c.FollowUpSeconds <= 0 {
		c.FollowUpSeconds = defaultFollowUpSeconds
	}
	if c.STT.Provider == "" {
		c.STT.Provider = providerDeepgram
	}
	if c.TTS.Provider == "" {
		c.TTS.Provider = providerElevenLabs
	}
	if c.ControlPort <= 0 {
		c.ControlPort = defaultControlPort
	}
	// The phone's managed server defaults to another port; ours is pinned
	// here so both channels can serve local speech on one machine.
	if c.SpeechServer.Port <= 0 {
		c.SpeechServer.Port = defaultSpeechPort
	}
	if c.OutputVolume <= 0 {
		c.OutputVolume = 100
	}
	c.UnknownSpeaker = strings.ToLower(strings.TrimSpace(c.UnknownSpeaker))
	if c.UnknownSpeaker == "" {
		c.UnknownSpeaker = unknownAnonymous
	}
	if c.SpeakerThreshold <= 0 {
		c.SpeakerThreshold = defaultSpeakerThreshold
	}
	if c.RoomTimeoutMinutes <= 0 {
		c.RoomTimeoutMinutes = defaultRoomTimeoutMinutes
	}
	if c.VADRatio <= 0 {
		c.VADRatio = defaultVADRatio
	}
	if c.BargeRatio <= 0 {
		c.BargeRatio = defaultBargeRatio
	}
	if c.SilenceMs <= 0 {
		c.SilenceMs = defaultSilenceMs
	}
	c.STT.BaseURL = strings.TrimSuffix(strings.TrimSpace(c.STT.BaseURL), "/")
	c.TTS.BaseURL = strings.TrimSuffix(strings.TrimSpace(c.TTS.BaseURL), "/")
	// A local tier with no server named is Factor's own, the case the wizard
	// writes: resolving it here means validation, the startup probe, and the
	// speech clients all treat it as any other OpenAI-compatible server.
	if c.STT.Provider == providerLocalOpenAI && c.STT.BaseURL == "" {
		c.STT.BaseURL = phone.SpeechBaseURL(c.SpeechServer)
	}
	if c.TTS.Provider == providerLocalOpenAI && c.TTS.BaseURL == "" {
		c.TTS.BaseURL = phone.SpeechBaseURL(c.SpeechServer)
	}
	c.STTAPIBase = strings.TrimSuffix(strings.TrimSpace(c.STTAPIBase), "/")
	c.TTSAPIBase = strings.TrimSuffix(strings.TrimSpace(c.TTSAPIBase), "/")
}

// validate rejects a section that could only fail later, mid-conversation.
func (c Config) validate() error {
	switch c.Activation {
	case activationAlways, activationWakeWord, activationPTT:
	default:
		return fmt.Errorf("unknown activation %q (want %s, %s, or %s)",
			c.Activation, activationAlways, activationWakeWord, activationPTT)
	}
	switch c.STT.Provider {
	case providerDeepgram, providerWhisper:
		if c.STTAPIKey == "" {
			return fmt.Errorf("stt_api_key is required for stt.provider %q", c.STT.Provider)
		}
	case providerLocalOpenAI:
		if c.STT.BaseURL == "" {
			return fmt.Errorf("stt.base_url is required for a local speech-to-text server")
		}
	default:
		return fmt.Errorf("unknown stt.provider %q (want %s, %s, or %s)",
			c.STT.Provider, providerDeepgram, providerWhisper, providerLocalOpenAI)
	}
	switch c.TTS.Provider {
	case providerElevenLabs:
		if c.ElevenLabsAPIKey == "" {
			return fmt.Errorf("elevenlabs_api_key is required for tts.provider %q", providerElevenLabs)
		}
	case providerLocalOpenAI:
		if c.TTS.BaseURL == "" {
			return fmt.Errorf("tts.base_url is required for a local text-to-speech server")
		}
	default:
		return fmt.Errorf("unknown tts.provider %q (want %s or %s)",
			c.TTS.Provider, providerElevenLabs, providerLocalOpenAI)
	}
	if c.OutputVolume > 100 {
		return fmt.Errorf("output_volume %d is out of range (want 1–100)", c.OutputVolume)
	}
	if c.SpeakerID && !c.managedSpeech() {
		return fmt.Errorf("speaker_id needs the managed local speech server, which computes the voice embeddings " +
			"(pick a local stt or tts tier and leave its base_url blank)")
	}
	switch c.UnknownSpeaker {
	case unknownAnonymous, unknownEnroll:
	default:
		return fmt.Errorf("unknown unknown_speaker %q (want %s or %s)",
			c.UnknownSpeaker, unknownAnonymous, unknownEnroll)
	}
	if c.SpeakerThreshold > 1 {
		return fmt.Errorf("speaker_threshold %v is out of range (cosine similarity, want 0–1)", c.SpeakerThreshold)
	}
	// Asking for room isolation without speaker_id is asking for a boundary
	// nothing can draw: with no voice profiles every utterance is the same
	// person, so the room could never fill. Refusing is the honest answer —
	// quietly running without isolation would leave the user believing a
	// privacy setting is in force when it is not.
	if c.RoomIsolation != nil && *c.RoomIsolation && !c.SpeakerID {
		return fmt.Errorf("room_isolation needs speaker_id: without voice profiles there is no way to tell " +
			"a guest's voice from the owner's")
	}
	if c.managedSpeech() && c.SpeechServer.Port == c.ControlPort {
		return fmt.Errorf("speech_server.port %d collides with control_port", c.SpeechServer.Port)
	}
	return c.SpeechServer.Validate()
}

// roomIsolation reports whether turns should be scoped to the room's audience.
// It cannot run without speaker_id, so an explicit yes there is refused by
// validate rather than silently ignored here; unset means on wherever
// speaker_id is, which is the whole reason someone turns speaker_id on.
func (c *Config) roomIsolation() bool {
	if !c.SpeakerID {
		return false
	}
	return c.RoomIsolation == nil || *c.RoomIsolation
}

// localSTT and localTTS report which halves of the pipeline run on this
// machine, which decides the weights the speech server has to load.
func (c Config) localSTT() bool { return c.STT.Provider == providerLocalOpenAI }
func (c Config) localTTS() bool { return c.TTS.Provider == providerLocalOpenAI }

// managedSpeech reports whether a local tier is served by the speech server
// Factor runs itself, rather than one the user pointed it at.
func (c Config) managedSpeech() bool {
	managed := phone.SpeechBaseURL(c.SpeechServer)
	return (c.localSTT() && c.STT.BaseURL == managed) || (c.localTTS() && c.TTS.BaseURL == managed)
}

// Tier reports which of the documented speech tiers this config selects:
// 1 all-cloud audio, 2 local speech-to-text, 3 local text-to-speech,
// 4 fully local audio.
func (c Config) Tier() int {
	switch {
	case c.localSTT() && c.localTTS():
		return 4
	case c.localTTS():
		return 3
	case c.localSTT():
		return 2
	default:
		return 1
	}
}

// TierLabel describes the tier for status output and the setup wizard.
func (c Config) TierLabel() string {
	switch c.Tier() {
	case 4:
		return "tier 4 · fully local audio"
	case 3:
		return "tier 3 · local text-to-speech"
	case 2:
		return "tier 2 · local speech-to-text"
	default:
		return "tier 1 · cloud audio"
	}
}
