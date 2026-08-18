package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	"github.com/cyqlelabs/factor/internal/channel/phone"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// fakeMic scripts the microphone: frames the test feeds cross a channel into
// whatever capture stream is currently open. A nil frame kills the "helper",
// exercising the reopen path; the next Capture call reads on.
type fakeMic struct {
	frames chan []byte
	opens  atomic.Int32
}

func newFakeMic() *fakeMic { return &fakeMic{frames: make(chan []byte, 4096)} }

func (m *fakeMic) capture(ctx context.Context, _ []string) (io.ReadCloser, error) {
	m.opens.Add(1)
	return &micReader{mic: m, ctx: ctx, closed: make(chan struct{})}, nil
}

func (m *fakeMic) feed(frames ...[]byte) {
	for _, frame := range frames {
		m.frames <- frame
	}
}

// die makes the current capture stream fail, like a helper crashing.
func (m *fakeMic) die() { m.frames <- nil }

type micReader struct {
	mic    *fakeMic
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
	buf    []byte
}

func (r *micReader) Read(b []byte) (int, error) {
	if len(r.buf) == 0 {
		select {
		case frame := <-r.mic.frames:
			if frame == nil {
				return 0, errors.New("the capture helper died")
			}
			r.buf = frame
		case <-r.ctx.Done():
			return 0, io.EOF
		case <-r.closed:
			return 0, io.EOF
		}
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *micReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type turnCall struct {
	ctx     context.Context
	content string
	session string
}

// voiceHarness wires a Voice to a scripted machine: a fake microphone, a
// recording speaker, a fake speech API, and a scripted agent loop.
type voiceHarness struct {
	t       *testing.T
	v       *Voice
	bus     *bus.MessageBus
	api     *fakeSpeechAPI
	mic     *fakeMic
	speaker *fakeSpeaker
	turns   chan turnCall

	mu    sync.Mutex
	reply string
	err   error
	block bool // the runner waits for cancellation instead of answering
}

func newVoiceHarness(t *testing.T, mutate func(*Config)) *voiceHarness {
	t.Helper()
	t.Setenv("FACTOR_HOME", t.TempDir())
	h := &voiceHarness{
		t:       t,
		bus:     bus.New(),
		api:     newFakeSpeechAPI(t),
		mic:     newFakeMic(),
		speaker: &fakeSpeaker{},
		turns:   make(chan turnCall, 8),
		reply:   "as you wish",
	}

	cfg := Config{
		STT:         phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: h.api.URL},
		TTS:         phone.AudioEndpoint{Provider: providerLocalOpenAI, BaseURL: h.api.URL},
		ControlPort: freePort(t),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	v, err := New(cfg, h.bus)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v.env = Env{
		GOOS:    "linux",
		Has:     func(string) bool { return true },
		Getenv:  func(string) string { return "" },
		Glob:    func(string) ([]string, error) { return nil, nil },
		Capture: h.mic.capture,
		Play:    h.speaker.play,
	}
	v.BindTurnRunner(func(ctx context.Context, content, session string) (string, error) {
		h.mu.Lock()
		reply, err, block := h.reply, h.err, h.block
		h.mu.Unlock()
		h.turns <- turnCall{ctx: ctx, content: content, session: session}
		if block {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return reply, err
	})
	h.v = v
	return h
}

func (h *voiceHarness) start() {
	h.t.Helper()
	if err := h.v.Start(context.Background()); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	h.t.Cleanup(func() { _ = h.v.Stop() })
	// The channel is deaf until the tier is resolved and the mic is open.
	waitUntil(h.t, func() bool { return h.v.ready.Load() })
}

// say feeds one spoken utterance: settle frames, speech, closing silence.
func (h *voiceHarness) say() {
	h.mic.feed(repeat(silenceFrame(), 20)...)
	h.mic.feed(repeat(toneFrame(8000), 30)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)
}

func (h *voiceHarness) turn(within time.Duration) turnCall {
	h.t.Helper()
	select {
	case call := <-h.turns:
		return call
	case <-time.After(within):
		h.t.Fatal("no turn arrived")
		return turnCall{}
	}
}

func (h *voiceHarness) noTurn(within time.Duration) {
	h.t.Helper()
	select {
	case call := <-h.turns:
		h.t.Fatalf("an unexpected turn ran: %q", call.content)
	case <-time.After(within):
	}
}

// synthesized lists every text handed to the TTS endpoint.
func (h *voiceHarness) synthesized() []string {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	var texts []string
	for _, body := range h.api.bodies {
		if input, ok := body["input"].(string); ok {
			texts = append(texts, input)
		}
	}
	return texts
}

func (h *voiceHarness) setTranscript(text string) {
	h.api.mu.Lock()
	h.api.text = text
	h.api.mu.Unlock()
}

func (h *voiceHarness) setReplyPCM(pcm []byte) {
	h.api.mu.Lock()
	h.api.pcm = pcm
	h.api.mu.Unlock()
}

func TestVoiceRegistersAsAConnector(t *testing.T) {
	for _, name := range channel.Registered() {
		if name == "voice" {
			return
		}
	}
	t.Error("the voice connector never registered")
}

func TestVoiceFactoryDecodesItsOwnSection(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	built := channel.Build(map[string]json.RawMessage{
		"voice": json.RawMessage(`{"stt_api_key": "dg", "elevenlabs_api_key": "el"}`),
	}, bus.New())
	if len(built) != 1 || built[0].Name() != "voice" {
		t.Fatalf("built = %v", built)
	}
	// A section that cannot parse, and one that cannot validate, are both
	// skipped by Build rather than failing the gateway.
	for _, raw := range []string{`{"enabled": "yes"}`, `{"activation": "sometimes"}`} {
		if built := channel.Build(map[string]json.RawMessage{"voice": json.RawMessage(raw)}, bus.New()); len(built) != 0 {
			t.Errorf("section %s built a channel", raw)
		}
	}
}

// The bare wake word is attention, not a request: the agent answers with a
// short prompt instead of running a turn.
func TestVoiceAcknowledgesTheBareWakeWord(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "wake-word" })
	h.start()
	h.setTranscript("Factor!")
	h.say()
	h.noTurn(2 * time.Second)
	waitUntil(t, func() bool {
		for _, text := range h.synthesized() {
			if text == "Yes?" {
				return true
			}
		}
		return false
	})
}

func TestVoiceSurvivesATranscriptionOutage(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.start()
	h.api.fail(http.StatusInternalServerError)
	h.say()
	h.noTurn(2 * time.Second)

	// The outage ends; the next utterance goes through.
	h.api.fail(http.StatusOK)
	h.say()
	h.turn(10 * time.Second)
}

// The whole path, no audio hardware: scripted PCM in, the transcript through
// the agent loop under the one session key, the reply synthesised and heard.
func TestVoiceRunsAnUtteranceThroughTheLoopAndSpeaksTheReply(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.start()
	h.say()

	call := h.turn(10 * time.Second)
	if call.session != "voice:local" {
		t.Errorf("session = %q", call.session)
	}
	if !contains(call.content, "hello there") {
		t.Errorf("turn content = %q, want the transcript", call.content)
	}
	// The first turn of a boot carries the voice brief; it tells the agent
	// its words will be spoken.
	if !contains(call.content, "voice_write") {
		t.Errorf("the first turn carries no brief: %q", call.content)
	}

	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })
	if texts := h.synthesized(); len(texts) == 0 || texts[len(texts)-1] != "as you wish" {
		t.Errorf("synthesized = %v, want the reply", texts)
	}

	// The brief is per boot, not per turn.
	h.say()
	if second := h.turn(10 * time.Second); contains(second.content, "voice_write") {
		t.Errorf("the second turn repeats the brief: %q", second.content)
	}
}

func TestVoiceWakeWordGatesAndStripsItself(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "wake-word" })
	h.mu.Lock()
	h.reply = "" // a blank reply is not spoken, so no follow-up window opens
	h.mu.Unlock()
	h.start()

	h.setTranscript("Factor, what time is it?")
	h.say()
	call := h.turn(10 * time.Second)
	if !contains(call.content, "what time is it?") || contains(call.content, "Factor,") {
		t.Errorf("turn content = %q, want the wake word stripped", call.content)
	}

	h.setTranscript("nothing for you")
	h.say()
	h.noTurn(2 * time.Second)
}

func TestVoiceBargeInReplacesTheReply(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.setReplyPCM(clip(120000)) // 2.5 s of reply: plenty of room to interrupt
	h.start()
	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })

	// Talk over it: the reply pauses at speech start, and once the utterance
	// gates in, playback is discarded and a new turn runs.
	h.setTranscript("actually, stop")
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)

	second := h.turn(10 * time.Second)
	if !contains(second.content, "actually, stop") {
		t.Errorf("the barge-in turn carries %q", second.content)
	}
	// The first reply's helper run ended short of the clip: it was killed at
	// the barge, and never resumed.
	if first := h.speaker.runLength(0); first >= 120000 {
		t.Errorf("the interrupted reply still played all %d bytes", first)
	}
}

func TestVoiceBargeInCancelsATurnStillThinking(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.mu.Lock()
	h.block = true // the runner answers only by being cancelled
	h.mu.Unlock()
	h.start()

	h.say()
	first := h.turn(10 * time.Second)

	h.mu.Lock()
	h.block = false
	h.mu.Unlock()
	h.say()
	h.turn(10 * time.Second)

	select {
	case <-first.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("the superseded turn was never cancelled")
	}
}

// A rejected barge-in — background chatter in wake-word mode — resumes the
// reply instead of abandoning it.
func TestVoiceRejectedBargeInResumesTheReply(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) {
		c.Activation = "wake-word"
		c.FollowUpSeconds = 1 // the window after the reply must not mask the rejection
	})
	pcm := clip(120000)
	h.setReplyPCM(pcm)
	h.start()

	h.setTranscript("factor hello")
	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })
	time.Sleep(1100 * time.Millisecond) // let the wake-word window lapse

	h.setTranscript("unrelated room noise")
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)

	h.noTurn(2 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) >= len(pcm) })
}

func TestVoicePushToTalkOnlyHearsWhenArmed(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "push-to-talk" })
	h.start()

	h.say()
	h.noTurn(2 * time.Second)

	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/ptt", h.v.cfg.ControlPort), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ptt = HTTP %d", resp.StatusCode)
	}

	h.say()
	if call := h.turn(10 * time.Second); !contains(call.content, "hello there") {
		t.Errorf("the armed utterance carried %q", call.content)
	}
}

func TestVoiceSendSpeaksProactiveMessagesAndDropsInterims(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.start()

	if err := h.v.Send(context.Background(), bus.OutboundMessage{Interim: true, Content: "thinking…"}); err != nil {
		t.Errorf("interim = %v", err)
	}
	if err := h.v.Send(context.Background(), bus.OutboundMessage{Content: "the job finished"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitUntil(t, func() bool {
		for _, text := range h.synthesized() {
			if text == "the job finished" {
				return true
			}
		}
		return false
	})
	for _, text := range h.synthesized() {
		if text == "thinking…" {
			t.Error("an interim note was spoken")
		}
	}
}

func TestVoiceSendRefusesBeforeTheChannelIsReady(t *testing.T) {
	h := newVoiceHarness(t, nil)
	if err := h.v.Send(context.Background(), bus.OutboundMessage{Content: "x"}); err == nil {
		t.Error("Send before Start should fail so the manager retries")
	}
}

func TestVoiceReopensTheMicrophoneAfterAHelperCrash(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.start()
	h.mic.die()
	waitUntil(t, func() bool { return h.mic.opens.Load() >= 2 })
	// The channel still works end to end on the reopened stream.
	waitUntil(t, func() bool { return h.v.ready.Load() })
	h.say()
	h.turn(10 * time.Second)
}

// micHealth reads the control endpoint's microphone gauges.
func micHealth(t *testing.T, port int) (level, floor float64, silent bool) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		MicLevel  float64 `json:"mic_level"`
		MicFloor  float64 `json:"mic_floor"`
		MicSilent bool    `json:"mic_silent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.MicLevel, body.MicFloor, body.MicSilent
}

// The health endpoint answers the first question of a mute session: is any
// signal reaching the code at all?
func TestVoiceHealthReportsMicrophoneGauges(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.start()

	h.mic.feed(repeat(toneFrame(2000), 10)...)
	waitUntil(t, func() bool {
		level, floor, _ := micHealth(t, h.v.cfg.ControlPort)
		return level > 0 && floor > 0
	})
}

// An unbroken run of exact-zero samples is the signature of capturing the
// wrong device; the channel must say so instead of listening to nothing.
func TestVoiceReportsADigitallySilentMicrophone(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.start()

	h.mic.feed(repeat(silenceFrame(), 10*1000/frameMs+5)...)
	waitUntil(t, func() bool {
		_, _, silent := micHealth(t, h.v.cfg.ControlPort)
		return silent
	})

	// Real signal clears the flag.
	h.mic.feed(repeat(toneFrame(500), 5)...)
	waitUntil(t, func() bool {
		_, _, silent := micHealth(t, h.v.cfg.ControlPort)
		return !silent
	})
}

func TestVoiceStartFailures(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	cfg := validConfig()
	cfg.ControlPort = freePort(t)

	unbound, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := unbound.Start(context.Background()); err == nil || !contains(err.Error(), "not attached") {
		t.Errorf("Start without a loop = %v", err)
	}

	deaf, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	deaf.BindTurnRunner(func(context.Context, string, string) (string, error) { return "", nil })
	deaf.env = scriptedEnv("linux") // no helpers at all
	if err := deaf.Start(context.Background()); err == nil || !contains(err.Error(), "helper") {
		t.Errorf("Start without helpers = %v", err)
	}

	taken, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.ControlPort))
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	blocked, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked.BindTurnRunner(func(context.Context, string, string) (string, error) { return "", nil })
	blocked.env = scriptedEnv("linux", "parec", "paplay")
	if err := blocked.Start(context.Background()); err == nil || !contains(err.Error(), "control") {
		t.Errorf("Start on a taken port = %v", err)
	}
}

func TestVoiceChannelShape(t *testing.T) {
	h := newVoiceHarness(t, nil)
	if h.v.Name() != "voice" {
		t.Errorf("Name = %q", h.v.Name())
	}
	if h.v.MaxMessageLength() != 0 {
		t.Errorf("MaxMessageLength = %d, want unlimited", h.v.MaxMessageLength())
	}
	if !bus.External("voice") {
		t.Error("voice must be an external channel: proactive messages can reach it")
	}
}

func TestGateDecisions(t *testing.T) {
	build := func(activation string) *Voice {
		t.Setenv("FACTOR_HOME", t.TempDir())
		cfg := validConfig()
		cfg.Activation = activation
		v, err := New(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	always := build("always")
	if dec := always.gate("anything"); !dec.accept || dec.text != "anything" {
		t.Errorf("always: %+v", dec)
	}
	if dec := always.gate("  "); dec.accept {
		t.Error("an empty transcript was accepted")
	}

	ptt := build("push-to-talk")
	if dec := ptt.gate("anything"); dec.accept {
		t.Error("push-to-talk accepted without being armed")
	}
	ptt.mu.Lock()
	ptt.pttUntil = time.Now().Add(time.Minute)
	ptt.mu.Unlock()
	if dec := ptt.gate("now"); !dec.accept {
		t.Error("an armed push-to-talk rejected the utterance")
	}
	if dec := ptt.gate("again"); dec.accept {
		t.Error("one arm admitted two utterances")
	}

	wake := build("wake-word")
	if dec := wake.gate("factor do the thing"); !dec.accept || dec.text != "do the thing" {
		t.Errorf("wake word: %+v", dec)
	}
	if dec := wake.gate("do the thing"); dec.accept {
		t.Error("wake-word mode accepted an unaddressed utterance")
	}
	if dec := wake.gate("Factor"); !dec.acknowledge || dec.accept {
		t.Errorf("the bare wake word should acknowledge: %+v", dec)
	}
	// The bare wake word opened the follow-up window.
	if dec := wake.gate("do the thing"); !dec.accept {
		t.Error("the follow-up window did not admit the next utterance")
	}
	wake.mu.Lock()
	wake.windowUntil = time.Time{}
	wake.mu.Unlock()
	if dec := wake.gate("do the thing"); dec.accept {
		t.Error("a closed window still admitted utterances")
	}
	// Push-to-talk arms in wake-word mode too: the misfire rescue.
	wake.ArmPTT()
	if dec := wake.gate("no wake word here"); !dec.accept {
		t.Error("push-to-talk did not override the wake word")
	}
}

func TestStripWakeWord(t *testing.T) {
	cases := []struct {
		text, wake string
		want       string
		ok         bool
	}{
		{"Factor, open the browser", "factor", "open the browser", true},
		{"hey factor status report", "factor", "status report", true},
		{"FACTOR!", "factor", "", true},
		{"hey Jarvis do it", "hey jarvis", "do it", true},
		{"refactor this function", "factor", "", false},
		// The wake word may sit second ("hey factor"), which makes this a
		// deliberate false positive; push-to-talk is the documented rescue.
		{"the factor of two", "factor", "of two", true},
		{"and the factor of two", "factor", "", false}, // but no deeper than second
		{"completely unrelated", "factor", "", false},
		{"factor", "", "", false},
	}
	for _, tc := range cases {
		got, ok := stripWakeWord(tc.text, tc.wake)
		if got != tc.want || ok != tc.ok {
			t.Errorf("stripWakeWord(%q, %q) = %q, %v; want %q, %v", tc.text, tc.wake, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSpokenLinesAreLocalized(t *testing.T) {
	if !contains(spokenFailure("es-MX"), "Perdón") || contains(spokenFailure("en"), "Perdón") {
		t.Error("spokenFailure localization")
	}
	if ackLine("es") != "¿Sí?" || ackLine("en") != "Yes?" {
		t.Error("ackLine localization")
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
