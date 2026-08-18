package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/channel/phone"
)

// The speech clients. The phone channel's Python shell owns its own STT/TTS
// adapters; here Go is the client, speaking the same three dialects: the
// OpenAI speech protocol (the managed server, any local server, and OpenAI's
// own whisper), Deepgram's REST transcription, and ElevenLabs' REST
// synthesis. Every synthesiser is asked for headerless 24 kHz s16le mono, so
// one playback pipeline serves all of them.

const (
	deepgramAPIBase   = "https://api.deepgram.com"
	openaiAPIBase     = "https://api.openai.com/v1"
	elevenLabsAPIBase = "https://api.elevenlabs.io"

	deepgramModel   = "nova-3"
	whisperModel    = "whisper-1"
	elevenLabsModel = "eleven_flash_v2_5"

	// localTTSModel names the protocol rather than the weights, exactly as on
	// the phone tier: the voice is chosen on the speech server.
	localTTSModel = "tts-1"

	// defaultElevenLabsVoice is ElevenLabs' stock "Rachel" voice, used when no
	// voice id is configured — the REST endpoint, unlike Patter, has no
	// default of its own.
	defaultElevenLabsVoice = "21m00Tcm4TlvDq8ikWAM"

	sttTimeout = 30 * time.Second
	ttsTimeout = 60 * time.Second

	// maxSpeechResponse bounds what a speech API may hand back — minutes of
	// PCM, never gigabytes.
	maxSpeechResponse = 32 << 20
)

// speechClient runs both halves of the pipeline for one resolved config.
type speechClient struct {
	cfg   Config
	token string // this boot's secret, sent only to the managed server
	httpc *http.Client
}

func newSpeechClient(cfg Config, token string) *speechClient {
	return &speechClient{cfg: cfg, token: token, httpc: &http.Client{Timeout: ttsTimeout}}
}

// transcribe turns one utterance of 16 kHz PCM into text.
func (s *speechClient) transcribe(ctx context.Context, pcm []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sttTimeout)
	defer cancel()
	switch s.cfg.STT.Provider {
	case providerDeepgram:
		return s.transcribeDeepgram(ctx, pcm)
	case providerWhisper:
		base := s.cfg.STTAPIBase
		if base == "" {
			base = openaiAPIBase
		}
		model := s.cfg.STT.Model
		if model == "" {
			model = whisperModel
		}
		return s.transcribeOpenAI(ctx, base, model, s.cfg.STTAPIKey, pcm)
	default: // local-openai
		token := ""
		if s.cfg.managedSpeech() {
			token = s.token
		}
		return s.transcribeOpenAI(ctx, s.cfg.STT.BaseURL, s.cfg.STT.Model, token, pcm)
	}
}

// transcribeOpenAI speaks the OpenAI transcription protocol, which the
// managed server, user-run local servers, and OpenAI itself all accept.
func (s *speechClient) transcribeOpenAI(ctx context.Context, base, model, key string, pcm []byte) (string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wavPCM(pcm, captureRate)); err != nil {
		return "", err
	}
	if model != "" {
		if err := form.WriteField("model", model); err != nil {
			return "", err
		}
	}
	if err := form.WriteField("language", s.cfg.Language); err != nil {
		return "", err
	}
	if err := form.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	raw, err := s.do(req)
	if err != nil {
		return "", fmt.Errorf("transcription: %w", err)
	}
	var reply struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("transcription reply: %w", err)
	}
	return strings.TrimSpace(reply.Text), nil
}

func (s *speechClient) transcribeDeepgram(ctx context.Context, pcm []byte) (string, error) {
	base := s.cfg.STTAPIBase
	if base == "" {
		base = deepgramAPIBase
	}
	model := s.cfg.STT.Model
	if model == "" {
		model = deepgramModel
	}
	url := fmt.Sprintf("%s/v1/listen?model=%s&language=%s", base, model, s.cfg.Language)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(wavPCM(pcm, captureRate)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Token "+s.cfg.STTAPIKey)
	req.Header.Set("Content-Type", "audio/wav")
	raw, err := s.do(req)
	if err != nil {
		return "", fmt.Errorf("transcription: %w", err)
	}
	var reply struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("transcription reply: %w", err)
	}
	if len(reply.Results.Channels) == 0 || len(reply.Results.Channels[0].Alternatives) == 0 {
		return "", nil
	}
	return strings.TrimSpace(reply.Results.Channels[0].Alternatives[0].Transcript), nil
}

// synthesize turns text into 24 kHz s16le mono PCM.
func (s *speechClient) synthesize(ctx context.Context, text string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ttsTimeout)
	defer cancel()
	if s.cfg.TTS.Provider == providerElevenLabs {
		return s.synthesizeElevenLabs(ctx, text)
	}
	return s.synthesizeOpenAI(ctx, text)
}

func (s *speechClient) synthesizeOpenAI(ctx context.Context, text string) ([]byte, error) {
	model := s.cfg.TTS.Model
	if model == "" {
		model = localTTSModel
	}
	payload, err := json.Marshal(map[string]string{
		"model": model, "input": text, "voice": s.cfg.TTS.Voice, "response_format": "pcm",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.TTS.BaseURL+"/audio/speech", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.managedSpeech() {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	pcm, err := s.do(req)
	if err != nil {
		return nil, fmt.Errorf("synthesis: %w", err)
	}
	return pcm, nil
}

func (s *speechClient) synthesizeElevenLabs(ctx context.Context, text string) ([]byte, error) {
	base := s.cfg.TTSAPIBase
	if base == "" {
		base = elevenLabsAPIBase
	}
	voice := s.cfg.TTS.Voice
	if voice == "" {
		voice = s.cfg.VoiceID
	}
	if voice == "" {
		voice = defaultElevenLabsVoice
	}
	model := s.cfg.TTS.Model
	if model == "" {
		model = elevenLabsModel
	}
	payload, err := json.Marshal(map[string]string{"text": text, "model_id": model})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/text-to-speech/%s?output_format=pcm_24000", base, voice)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", s.cfg.ElevenLabsAPIKey)
	pcm, err := s.do(req)
	if err != nil {
		return nil, fmt.Errorf("synthesis: %w", err)
	}
	return pcm, nil
}

// do runs one speech request, folding a non-2xx status and the start of its
// body into the error.
func (s *speechClient) do(req *http.Request) ([]byte, error) {
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, detail)
	}
	return body, nil
}

// resolveAudioTier checks the local speech servers a config asks for, exactly
// as the phone does: an unreachable one falls back to the cloud tier when
// allowed and credentialed, and takes the channel down otherwise.
func resolveAudioTier(ctx context.Context, cfg Config) (Config, error) {
	if cfg.localSTT() {
		if err := phone.ProbeSpeechServer(ctx, cfg.STT.BaseURL); err != nil {
			if !cfg.localAudioFallback() {
				return cfg, fmt.Errorf("local speech-to-text server at %s is unreachable: %w", cfg.STT.BaseURL, err)
			}
			if cfg.STTAPIKey == "" {
				return cfg, fmt.Errorf("local speech-to-text server at %s is unreachable and there is no stt_api_key to fall back to the cloud with: %w",
					cfg.STT.BaseURL, err)
			}
			slog.Warn("local speech-to-text server unreachable; falling back to the cloud tier",
				"base_url", cfg.STT.BaseURL, "error", err)
			cfg.STT = phone.AudioEndpoint{Provider: providerDeepgram}
		}
	}
	if cfg.localTTS() {
		if err := phone.ProbeSpeechServer(ctx, cfg.TTS.BaseURL); err != nil {
			if !cfg.localAudioFallback() {
				return cfg, fmt.Errorf("local text-to-speech server at %s is unreachable: %w", cfg.TTS.BaseURL, err)
			}
			if cfg.ElevenLabsAPIKey == "" {
				return cfg, fmt.Errorf("local text-to-speech server at %s is unreachable and there is no elevenlabs_api_key to fall back to the cloud with: %w",
					cfg.TTS.BaseURL, err)
			}
			slog.Warn("local text-to-speech server unreachable; falling back to the cloud tier",
				"base_url", cfg.TTS.BaseURL, "error", err)
			cfg.TTS = phone.AudioEndpoint{Provider: providerElevenLabs, Voice: cfg.VoiceID}
		}
	}
	return cfg, nil
}
