package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/channel/phone"
)

// fakeSpeechAPI serves every dialect the clients speak and records what it
// was asked, so tests can assert on the exact request that crossed the wire.
type fakeSpeechAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	forms    []map[string]string
	bodies   []map[string]any
	text     string // transcription reply
	pcm      []byte // synthesis reply
	status   int
	hold     chan struct{} // when non-nil, every handler waits here first
}

// stall makes every request wait until release, like a speech server still
// loading its models.
func (f *fakeSpeechAPI) stall() {
	f.mu.Lock()
	f.hold = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeSpeechAPI) release() {
	f.mu.Lock()
	if f.hold != nil {
		close(f.hold)
		f.hold = nil
	}
	f.mu.Unlock()
}

func newFakeSpeechAPI(t *testing.T) *fakeSpeechAPI {
	t.Helper()
	f := &fakeSpeechAPI{text: "hello there", pcm: []byte("pcm-audio"), status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		hold := f.hold
		f.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		status := f.status
		var form map[string]string
		var body map[string]any
		switch {
		case strings.HasSuffix(r.URL.Path, "/audio/transcriptions"):
			_ = r.ParseMultipartForm(32 << 20)
			form = map[string]string{}
			for key, values := range r.MultipartForm.Value {
				form[key] = values[0]
			}
			if _, header, err := r.FormFile("file"); err == nil {
				form["file"] = header.Filename
			}
			f.forms = append(f.forms, form)
		default:
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.bodies = append(f.bodies, body)
		}
		text, pcm := f.text, f.pcm
		f.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/audio/transcriptions"):
			_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
		case strings.HasSuffix(r.URL.Path, "/listen"):
			_, _ = fmt.Fprintf(w,
				`{"results":{"channels":[{"alternatives":[{"transcript":%q}]}]}}`, text)
		default:
			_, _ = w.Write(pcm)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSpeechAPI) lastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeSpeechAPI) lastForm() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forms[len(f.forms)-1]
}

func (f *fakeSpeechAPI) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeSpeechAPI) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func clientFor(t *testing.T, mutate func(*Config)) (*speechClient, *fakeSpeechAPI) {
	t.Helper()
	api := newFakeSpeechAPI(t)
	cfg := validConfig()
	cfg.STTAPIBase = api.URL
	cfg.TTSAPIBase = api.URL
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.applyDefaults()
	return newSpeechClient(cfg, "boot-token"), api
}

func TestTranscribeDeepgramSendsWavWithTokenAuth(t *testing.T) {
	client, api := clientFor(t, nil)
	text, err := client.transcribe(context.Background(), toneFrame(1000))
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello there" {
		t.Errorf("text = %q", text)
	}
	req := api.lastRequest()
	if req.Header.Get("Authorization") != "Token dg-key" {
		t.Errorf("auth = %q, want Deepgram's Token scheme", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("content type = %q", req.Header.Get("Content-Type"))
	}
	query := req.URL.Query()
	if query.Get("model") != deepgramModel || query.Get("language") != "en" {
		t.Errorf("query = %v", query)
	}
}

// The managed server gets this boot's secret; a server the user pointed us at
// gets nothing — the same rule the phone channel enforces.
func TestTranscribeLocalOpenAISendsTheBootSecretOnlyToTheManagedServer(t *testing.T) {
	api := newFakeSpeechAPI(t)
	managed := Config{
		STT:              phone.AudioEndpoint{Provider: providerLocalOpenAI, Model: "base"},
		SpeechServer:     phone.SpeechConfig{Port: portOf(t, api.URL)},
		ElevenLabsAPIKey: "el",
	}
	managed.applyDefaults() // resolves the blank base_url to the managed server: the fake
	if !managed.managedSpeech() {
		t.Fatal("test setup: the fake is not seen as managed")
	}
	client := newSpeechClient(managed, "boot-token")
	if _, err := client.transcribe(context.Background(), toneFrame(1000)); err != nil {
		t.Fatal(err)
	}
	if auth := api.lastRequest().Header.Get("Authorization"); auth != "Bearer boot-token" {
		t.Errorf("managed auth = %q, want the boot secret", auth)
	}
	form := api.lastForm()
	if form["model"] != "base" || form["language"] != "en" || form["file"] == "" {
		t.Errorf("form = %v", form)
	}

	byo := Config{
		STT:              phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: api.URL},
		ElevenLabsAPIKey: "el",
	}
	byo.applyDefaults()
	if _, err := newSpeechClient(byo, "boot-token").transcribe(context.Background(), toneFrame(1000)); err != nil {
		t.Fatal(err)
	}
	if auth := api.lastRequest().Header.Get("Authorization"); auth != "" {
		t.Errorf("a user-run server was handed credentials: %q", auth)
	}
}

func TestTranscribeWhisperUsesTheCloudKeyAndDefaultModel(t *testing.T) {
	client, api := clientFor(t, func(c *Config) {
		c.STT = phone.AudioEndpoint{Provider: providerWhisper}
		c.STTAPIKey = "sk-openai"
	})
	if _, err := client.transcribe(context.Background(), toneFrame(1000)); err != nil {
		t.Fatal(err)
	}
	if auth := api.lastRequest().Header.Get("Authorization"); auth != "Bearer sk-openai" {
		t.Errorf("auth = %q", auth)
	}
	if form := api.lastForm(); form["model"] != whisperModel {
		t.Errorf("model = %q", form["model"])
	}
}

func TestSynthesizeElevenLabsAsksForPCM24k(t *testing.T) {
	client, api := clientFor(t, func(c *Config) { c.VoiceID = "my-voice" })
	pcm, err := client.synthesize(context.Background(), "hi!")
	if err != nil {
		t.Fatal(err)
	}
	if string(pcm) != "pcm-audio" {
		t.Errorf("pcm = %q", pcm)
	}
	req := api.lastRequest()
	if !strings.Contains(req.URL.Path, "/v1/text-to-speech/my-voice") {
		t.Errorf("path = %q, want the configured voice", req.URL.Path)
	}
	if req.URL.Query().Get("output_format") != "pcm_24000" {
		t.Errorf("output_format = %q", req.URL.Query().Get("output_format"))
	}
	if req.Header.Get("xi-api-key") != "el-key" {
		t.Errorf("api key header = %q", req.Header.Get("xi-api-key"))
	}
	body := api.lastBody()
	if body["text"] != "hi!" || body["model_id"] != elevenLabsModel {
		t.Errorf("body = %v", body)
	}
}

func TestSynthesizeElevenLabsFallsBackToTheStockVoice(t *testing.T) {
	client, api := clientFor(t, nil)
	if _, err := client.synthesize(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.lastRequest().URL.Path, defaultElevenLabsVoice) {
		t.Errorf("path = %q, want the stock voice", api.lastRequest().URL.Path)
	}
}

func TestSynthesizeLocalOpenAIRequestsHeaderlessPCM(t *testing.T) {
	api := newFakeSpeechAPI(t)
	cfg := Config{
		TTS:       phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: api.URL, Voice: "es_MX-ald-medium"},
		STTAPIKey: "dg",
	}
	cfg.applyDefaults()
	client := newSpeechClient(cfg, "boot-token")
	if _, err := client.synthesize(context.Background(), "hola"); err != nil {
		t.Fatal(err)
	}
	body := api.lastBody()
	if body["response_format"] != "pcm" || body["model"] != localTTSModel || body["voice"] != "es_MX-ald-medium" {
		t.Errorf("body = %v", body)
	}
	if auth := api.lastRequest().Header.Get("Authorization"); auth != "" {
		t.Errorf("a user-run server was handed credentials: %q", auth)
	}
}

func TestSpeechClientSurfacesAPIFailures(t *testing.T) {
	client, api := clientFor(t, nil)
	api.fail(http.StatusUnauthorized)
	if _, err := client.transcribe(context.Background(), toneFrame(1000)); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Errorf("transcribe error = %v", err)
	}
	if _, err := client.synthesize(context.Background(), "hi"); err == nil ||
		!strings.Contains(err.Error(), "401") {
		t.Errorf("synthesize error = %v", err)
	}
}

func TestResolveAudioTierFallsBackToTheCloud(t *testing.T) {
	cfg := Config{
		STT:              phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:1/v1"},
		TTS:              phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:1/v1"},
		STTAPIKey:        "dg",
		ElevenLabsAPIKey: "el",
		VoiceID:          "my-voice",
	}
	cfg.applyDefaults()
	resolved, err := resolveAudioTier(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.STT.Provider != providerDeepgram || resolved.TTS.Provider != providerElevenLabs {
		t.Errorf("resolved to %q/%q", resolved.STT.Provider, resolved.TTS.Provider)
	}
	if resolved.TTS.Voice != "my-voice" {
		t.Errorf("the configured voice was lost in the fallback: %q", resolved.TTS.Voice)
	}
}

func TestResolveAudioTierRefusesWhenFallbackIsOffOrUncredentialed(t *testing.T) {
	off := false
	cfg := Config{
		STT:                phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:1/v1"},
		STTAPIKey:          "dg",
		ElevenLabsAPIKey:   "el",
		LocalAudioFallback: &off,
	}
	cfg.applyDefaults()
	if _, err := resolveAudioTier(context.Background(), cfg); err == nil {
		t.Error("fallback off: an unreachable server must take the channel down")
	}

	keyless := Config{
		TTS:       phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: "http://127.0.0.1:1/v1"},
		STTAPIKey: "dg",
	}
	keyless.applyDefaults()
	if _, err := resolveAudioTier(context.Background(), keyless); err == nil ||
		!strings.Contains(err.Error(), "elevenlabs_api_key") {
		t.Errorf("err = %v, want it to name the missing cloud key", err)
	}
}

func TestResolveAudioTierKeepsAReachableLocalServer(t *testing.T) {
	api := newFakeSpeechAPI(t)
	cfg := Config{
		STT:              phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: api.URL},
		ElevenLabsAPIKey: "el",
	}
	cfg.applyDefaults()
	resolved, err := resolveAudioTier(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.STT.Provider != providerLocalOpenAI {
		t.Errorf("a reachable local server was demoted to %q", resolved.STT.Provider)
	}
}

// portOf digs the TCP port out of an httptest URL.
func portOf(t *testing.T, url string) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(url[strings.LastIndex(url, ":"):], ":%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}
