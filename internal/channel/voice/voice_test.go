package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
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
	ctx      context.Context
	content  string
	session  string
	speaker  string
	audience string
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

	mu     sync.Mutex
	reply  string
	err    error
	block  bool   // the runner waits for cancellation instead of answering
	notice string // said on the way to the answer, as before a tool call
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
	v.BindTurnRunner(func(ctx context.Context, content, session, speaker, audience string, notice func(string)) (string, error) {
		h.mu.Lock()
		reply, err, block, note := h.reply, h.err, h.block, h.notice
		h.mu.Unlock()
		h.turns <- turnCall{ctx: ctx, content: content, session: session, speaker: speaker,
			audience: audience}
		if note != "" {
			notice(note)
			// The whole point of a filler line is that it is heard while the
			// turn works, so this turn does not answer until it has been.
			deadline := time.Now().Add(10 * time.Second)
			for !h.spoke(note) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		}
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

// spoke reports whether text has been handed to the TTS endpoint.
func (h *voiceHarness) spoke(text string) bool {
	for _, said := range h.synthesized() {
		if said == text {
			return true
		}
	}
	return false
}

func (h *voiceHarness) setNotice(text string) {
	h.mu.Lock()
	h.notice = text
	h.mu.Unlock()
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

func (h *voiceHarness) setEmbedding(embedding []float64) {
	h.api.mu.Lock()
	h.api.embedding = embedding
	h.api.mu.Unlock()
}

// enableSpeakerID points the managed speech endpoint at the fake API and
// turns identification on. Call before start.
func (h *voiceHarness) enableSpeakerID(policy string) {
	h.t.Helper()
	h.v.cfg.SpeechServer.Port = portOf(h.t, h.api.URL)
	h.v.cfg.SpeakerID = true
	h.v.cfg.SpeakerThreshold = defaultSpeakerThreshold
	h.v.cfg.UnknownSpeaker = policy
	h.v.speakers = newSpeakerStore(h.v.home)
}

// enableRoom turns audience scoping on. Like enableSpeakerID it reaches past
// New, whose validation wants the managed speech server the harness fakes.
func (h *voiceHarness) enableRoom(timeout time.Duration) {
	h.t.Helper()
	h.v.room = newRoom(timeout)
}

// waitQuiet waits for the reply in flight to finish sounding, so the next
// utterance is not a barge.
func (h *voiceHarness) waitQuiet() {
	h.t.Helper()
	waitUntil(h.t, func() bool { return !h.v.player.busy() && !h.v.turnInFlight() })
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
	// The turn carries the user's words and nothing else: the spoken-reply
	// briefing lives in the system prompt (channelBriefing), and anything
	// prepended here would be stored as the user's own testimony and mined
	// for entities by the memory engine.
	if call.content != "hello there" {
		t.Errorf("turn content = %q, want the bare transcript", call.content)
	}

	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })
	if texts := h.synthesized(); len(texts) == 0 || texts[len(texts)-1] != "as you wish" {
		t.Errorf("synthesized = %v, want the reply", texts)
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

// A barge-in transcript carries the agent's own words from the speakers
// before the user's; the wake word must count wherever it appears, or "Factor,
// stop" over a reply gets rejected and the reply talks on.
func TestVoiceBargeInHearsTheWakeWordMidTranscript(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) {
		c.Activation = "wake-word"
		c.FollowUpSeconds = 1
	})
	h.setReplyPCM(clip(120000))
	h.start()

	h.setTranscript("factor hello")
	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })
	time.Sleep(1100 * time.Millisecond) // the follow-up window lapses

	// What the mic actually hears: the reply from the speakers, then the user.
	h.setTranscript("and the weather tomorrow will be Factor stop talking")
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)

	if call := h.turn(10 * time.Second); !contains(call.content, "stop talking") ||
		contains(call.content, "weather tomorrow") {
		t.Errorf("the barge turn carried %q, want just what follows the wake word", call.content)
	}

	// Off playback the same mid-sentence mention stays rejected: someone
	// merely talking about Factor must not trigger it. Wait for the barge
	// turn's own reply to start and finish, and its follow-up window to
	// lapse, or this utterance would be a barge too.
	waitUntil(t, func() bool { return h.v.player.busy() })
	waitUntil(t, func() bool { return !h.v.player.busy() })
	time.Sleep(1100 * time.Millisecond)
	h.setTranscript("we discussed factor yesterday")
	h.say()
	h.noTurn(2 * time.Second)
}

// A barge-in does not need the wake word: it is spoken over the agent's own
// voice, so it is the word transcription mangles most — and talking over the
// reply is already the interruption.
func TestVoiceBargeInNeedsNoWakeWord(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) {
		c.Activation = "wake-word"
		c.FollowUpSeconds = 1 // the window must lapse, or it would mask the barge
	})
	h.setReplyPCM(clip(120000))
	h.start()

	h.setTranscript("factor hello")
	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })
	time.Sleep(1100 * time.Millisecond) // let the wake-word window lapse

	h.setTranscript("stop, delete them all")
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)

	if call := h.turn(10 * time.Second); !contains(call.content, "delete them all") {
		t.Errorf("the barge turn carried %q", call.content)
	}
}

// A barge-in the transcriber hears nothing in — a slammed door loud enough to
// open the mic — resumes the reply instead of abandoning it.
func TestVoiceUnintelligibleBargeInResumesTheReply(t *testing.T) {
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

	h.setTranscript("")
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)

	h.noTurn(2 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) >= len(pcm) })
}

// The reply leaking from the speakers back into the microphone is not the
// user: a barged utterance that is nothing but the agent's own recent words is
// discarded as echo and the reply plays on.
func TestVoiceIgnoresItsOwnEchoFromTheSpeakers(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.mu.Lock()
	h.reply = "the weather in madrid is sunny today"
	h.mu.Unlock()
	pcm := clip(120000)
	h.setReplyPCM(pcm)
	h.start()
	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })

	// What the mic hears is the reply itself, coming back off the walls.
	h.setTranscript("the weather in madrid is sunny today")
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)

	h.noTurn(2 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) >= len(pcm) })
}

// A real barge-in arrives with the reply's tail in front of it; only the
// user's words behind the echo become the turn.
func TestVoiceKeepsTheUserWordsBehindItsOwnEcho(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.setReplyPCM(clip(120000))
	h.start()
	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })

	h.setTranscript("as you wish please stop now")
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)

	call := h.turn(10 * time.Second)
	if call.content != "please stop now" {
		t.Errorf("the barge turn carried %q, want the user's words with the echo stripped", call.content)
	}
}

// output_volume scales the synthesized voice on its way to the speakers — the
// blunt lever against feedback when the room is loud.
func TestVoiceOutputVolumeScalesTheReply(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.OutputVolume = 50 })
	h.setReplyPCM(toneFrame(1000))
	h.start()
	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return len(h.speaker.heard()) >= frameBytes })

	heard := h.speaker.heard()
	if v := int16(binary.LittleEndian.Uint16(heard)); v != 500 {
		t.Errorf("first sample = %d, want half the synthesized 1000", v)
	}
}

// With speaker identification on and unknown voices enrolling, the first
// voice the machine hears becomes its owner and keeps the original session;
// a second, different voice holds its own conversation under its own key,
// with its words marked so the agent knows who is talking.
func TestVoiceSpeakerIdentificationSeparatesConversations(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.start()

	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	owner := h.turn(10 * time.Second)
	if owner.session != sessionKey || owner.content != "hello there" {
		t.Errorf("the owner's turn ran as %q in %q", owner.content, owner.session)
	}
	h.waitQuiet()

	// The owner again: same voice, same session, nothing marked.
	h.say()
	again := h.turn(10 * time.Second)
	if again.session != sessionKey || again.content != "hello there" {
		t.Errorf("the owner's second turn ran as %q in %q", again.content, again.session)
	}
	h.waitQuiet()

	// A different voice: enrolled on the spot, spoken to in its own session.
	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	guest := h.turn(10 * time.Second)
	if guest.session != sessionKey+":speaker-2" {
		t.Errorf("the guest's turn ran in %q", guest.session)
	}
	// The name travels beside the words, not inside them: what the guest
	// actually said is what reaches the loop, and the loop decides how to
	// show it to the model and how memory records it.
	if guest.content != "hello there" || guest.speaker != "speaker-2" {
		t.Errorf("the guest's turn arrived as content %q from speaker %q", guest.content, guest.speaker)
	}
	if owner.speaker != "" {
		t.Errorf("the owner's turn was attributed to %q; a house with one voice narrates nobody", owner.speaker)
	}
}

// An utterance too short to embed — half a second of "sí" — inherits the
// conversation's current speaker instead of falling back to the owner's
// session mid-dialogue.
func TestVoiceShortUtteranceInheritsTheCurrentSpeaker(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.start()

	// The owner enrolls, then a guest speaks a full sentence.
	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	h.turn(10 * time.Second)
	h.waitQuiet()
	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	if call := h.turn(10 * time.Second); call.session != sessionKey+":speaker-2" {
		t.Fatalf("the guest's turn ran in %q", call.session)
	}
	h.waitQuiet()

	// A short answer follows: too little voice to embed, but the guest is
	// still the one talking. The owner's embedding on the wire proves the
	// short path never asked for one.
	h.setEmbedding([]float64{1, 0, 0})
	h.mic.feed(repeat(silenceFrame(), 20)...)
	h.mic.feed(repeat(toneFrame(8000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)
	if call := h.turn(10 * time.Second); call.session != sessionKey+":speaker-2" {
		t.Errorf("the short follow-up ran in %q, want the guest's session", call.session)
	}
}

// The embedding endpoint answering nothing — a server without the speaker
// model, an utterance it could not read — leaves the turn running, unnamed.
func TestVoiceRunsTheTurnWhenTheEmbeddingIsUnavailable(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.start()

	h.setEmbedding(nil)
	h.say()
	call := h.turn(10 * time.Second)
	if call.session != sessionKey || call.content != "hello there" {
		t.Errorf("the unnamed turn ran as %q in %q", call.content, call.session)
	}
	if profiles := h.v.speakers.list(); len(profiles) != 0 {
		t.Errorf("an utterance nobody could embed was enrolled: %+v", profiles)
	}
}

// Under the anonymous policy an unknown voice is not enrolled and not named:
// the conversation stays the channel's original one.
func TestVoiceUnknownSpeakerStaysAnonymousByDefault(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownAnonymous)
	h.start()

	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	call := h.turn(10 * time.Second)
	if call.session != sessionKey || call.content != "hello there" {
		t.Errorf("an anonymous turn ran as %q in %q", call.content, call.session)
	}
	if profiles := h.v.speakers.list(); len(profiles) != 0 {
		t.Errorf("the anonymous policy enrolled %+v", profiles)
	}
}

// The speakers tool is how profiles get their real names: renaming defaults
// to whoever spoke last, so "soy Roxana" works without naming a profile.
func TestVoiceSpeakersTool(t *testing.T) {
	h := newVoiceHarness(t, nil)
	if len(h.v.Toolset()) != 1 {
		t.Error("the speakers tool exists without speaker_id")
	}
	h.enableSpeakerID(unknownEnroll)
	if len(h.v.Toolset()) != 2 {
		t.Error("speaker_id did not bring the speakers tool")
	}
	h.v.speakers.enroll(vec(1, 0))
	h.v.speakers.enroll(vec(0, 1))
	h.v.rememberSpeaker("speaker-2", viaMatch, 0.9)

	tool := &speakersTool{voice: h.v}
	if res := tool.Execute(context.Background(), map[string]any{
		"action": "rename", "new_name": "Roxana"}); res.IsError {
		t.Fatalf("rename: %s", res.ForLLM)
	}
	if name, _ := h.v.speakers.match(vec(0, 1)); name != "Roxana" {
		t.Errorf("the last speaker was not the one renamed; match = %q", name)
	}
	if h.v.lastSpeakerName() != "Roxana" {
		t.Errorf("the sticky speaker still answers to %q", h.v.lastSpeakerName())
	}

	listed := tool.Execute(context.Background(), map[string]any{"action": "list"})
	if !contains(listed.ForLLM, "speaker-1 (primary)") || !contains(listed.ForLLM, "Roxana") ||
		!contains(listed.ForLLM, "Last heard: Roxana") {
		t.Errorf("list = %q", listed.ForLLM)
	}

	if res := tool.Execute(context.Background(), map[string]any{
		"action": "forget", "name": "Roxana"}); res.IsError {
		t.Fatalf("forget: %s", res.ForLLM)
	}
	if name, _ := h.v.speakers.match(vec(0, 1)); name == "Roxana" {
		t.Error("a forgotten voice still matches")
	}
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

// A restart notice arrives the moment the gateway boots, while the local
// speech server is still loading its models: it must wait for the tier to
// resolve, not bounce off the channel as an error.
func TestVoiceSendDuringStartupWaitsForTheSpeechTier(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.api.stall()
	if err := h.v.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.v.Stop() })

	if err := h.v.Send(context.Background(), bus.OutboundMessage{Channel: "voice", Content: "back after the upgrade"}); err != nil {
		t.Fatalf("a message during startup was refused: %v", err)
	}
	h.api.release()
	waitUntil(t, func() bool { return h.spoke("back after the upgrade") })
}

// A channel whose Start never ran (or failed before the run context existed)
// must refuse the message, not accept it into a goroutine that panics.
func TestVoiceSendBeforeStartRefuses(t *testing.T) {
	h := newVoiceHarness(t, nil)
	if err := h.v.Send(context.Background(), bus.OutboundMessage{Channel: "voice", Content: "x"}); err == nil {
		t.Error("a never-started channel accepted a message")
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

// Silence is the only thing a spoken turn gives the user to go on, so what
// the agent says before reaching for a tool is spoken while it works — ahead
// of the answer it precedes, not after it.
func TestVoiceSpeaksWhatATurnSaysOnItsWayToTheAnswer(t *testing.T) {
	const note = "one moment, looking that up"
	h := newVoiceHarness(t, nil)
	h.setNotice(note)
	h.start()

	h.say()
	h.turn(10 * time.Second)
	waitUntil(t, func() bool { return h.spoke("as you wish") })

	var order []string
	for _, said := range h.synthesized() {
		if said == note || said == "as you wish" {
			order = append(order, said)
		}
	}
	if len(order) != 2 || order[0] != note || order[1] != "as you wish" {
		t.Errorf("spoken order = %q, want the note then the answer", order)
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
	deaf.BindTurnRunner(func(context.Context, string, string, string, string, func(string)) (string, error) { return "", nil })
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
	blocked.BindTurnRunner(func(context.Context, string, string, string, string, func(string)) (string, error) { return "", nil })
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

// gateNow judges an utterance that began this instant — the common case in
// these tests, where capture latency is not what is being exercised.
func (v *Voice) gateNow(text string, barged bool) decision {
	return v.gate(text, time.Now(), barged)
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
	if dec := always.gateNow("anything", false); !dec.accept || dec.text != "anything" {
		t.Errorf("always: %+v", dec)
	}
	if dec := always.gateNow("  ", false); dec.accept {
		t.Error("an empty transcript was accepted")
	}

	ptt := build("push-to-talk")
	if dec := ptt.gateNow("anything", false); dec.accept {
		t.Error("push-to-talk accepted without being armed")
	}
	ptt.mu.Lock()
	ptt.pttUntil = time.Now().Add(time.Minute)
	ptt.mu.Unlock()
	if dec := ptt.gateNow("now", false); !dec.accept {
		t.Error("an armed push-to-talk rejected the utterance")
	}
	if dec := ptt.gateNow("again", false); dec.accept {
		t.Error("one arm admitted two utterances")
	}

	wake := build("wake-word")
	if dec := wake.gateNow("factor do the thing", false); !dec.accept || dec.text != "do the thing" {
		t.Errorf("wake word: %+v", dec)
	}
	if dec := wake.gateNow("do the thing", false); dec.accept {
		t.Error("wake-word mode accepted an unaddressed utterance")
	}
	if dec := wake.gateNow("Factor", false); !dec.acknowledge || dec.accept {
		t.Errorf("the bare wake word should acknowledge: %+v", dec)
	}
	// The bare wake word opened the follow-up window.
	if dec := wake.gateNow("do the thing", false); !dec.accept {
		t.Error("the follow-up window did not admit the next utterance")
	}
	wake.mu.Lock()
	wake.windowUntil = time.Time{}
	wake.mu.Unlock()
	if dec := wake.gateNow("do the thing", false); dec.accept {
		t.Error("a closed window still admitted utterances")
	}
	// A long sentence begun inside the window is admitted even though the
	// window lapsed while it was being spoken and transcribed.
	wake.mu.Lock()
	wake.windowUntil = time.Now().Add(-time.Second)
	wake.mu.Unlock()
	if dec := wake.gate("do the thing", time.Now().Add(-2*time.Second), false); !dec.accept {
		t.Error("an utterance was judged by when transcription finished, not when it began")
	}
	// Push-to-talk arms in wake-word mode too: the misfire rescue.
	wake.ArmPTT()
	if dec := wake.gateNow("no wake word here", false); !dec.accept {
		t.Error("push-to-talk did not override the wake word")
	}
	// A barge-in — captured over the agent's own voice — is an interruption
	// in its own right: no wake word, no window.
	if dec := wake.gateNow("stop right there", true); !dec.accept || dec.text != "stop right there" {
		t.Errorf("barge-in without the wake word: %+v", dec)
	}
}

func TestStripWakeWord(t *testing.T) {
	cases := []struct {
		text, wake string
		anywhere   bool
		want       string
		ok         bool
	}{
		{"Factor, open the browser", "factor", false, "open the browser", true},
		{"hey factor status report", "factor", false, "status report", true},
		{"FACTOR!", "factor", false, "", true},
		{"hey Jarvis do it", "hey jarvis", false, "do it", true},
		{"refactor this function", "factor", false, "", false},
		// The wake word may sit second ("hey factor"), which makes this a
		// deliberate false positive; push-to-talk is the documented rescue.
		{"the factor of two", "factor", false, "of two", true},
		{"and the factor of two", "factor", false, "", false}, // but no deeper than second
		{"completely unrelated", "factor", false, "", false},
		{"factor", "", false, "", false},
		// A barge-in transcript opens with the speakers' own words, so the
		// wake word counts anywhere in it.
		{"the forecast for tomorrow says Factor stop the reply", "factor", true, "stop the reply", true},
		{"tomorrow will be sunny with FACTOR", "factor", true, "", true},
		{"tomorrow will be sunny all day", "factor", true, "", false},
	}
	for _, tc := range cases {
		got, ok := stripWakeWord(tc.text, tc.wake, tc.anywhere)
		if got != tc.want || ok != tc.ok {
			t.Errorf("stripWakeWord(%q, %q, %v) = %q, %v; want %q, %v", tc.text, tc.wake, tc.anywhere, got, ok, tc.want, tc.ok)
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

// At debug the channel prints every profile's score against the voice it just
// heard — the runners-up the decision alone does not report, and the numbers
// speaker_threshold is tuned against.
func TestVoiceLogsSpeakerScoresAtDebug(t *testing.T) {
	var out bytes.Buffer
	log.SetOutput(&out)
	slog.SetLogLoggerLevel(slog.LevelDebug)
	t.Cleanup(func() { log.SetOutput(os.Stderr); slog.SetLogLoggerLevel(slog.LevelInfo) })

	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.start()

	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	h.turn(10 * time.Second)
	h.waitQuiet()
	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	h.turn(10 * time.Second)

	logged := out.String()
	if !strings.Contains(logged, "speaker scores") || !strings.Contains(logged, "speaker-1=") {
		t.Errorf("the per-profile scores never reached the log:\n%s", logged)
	}
	if !strings.Contains(logged, "threshold=0.5") {
		t.Errorf("the scores line does not say what they were judged against:\n%s", logged)
	}
}
