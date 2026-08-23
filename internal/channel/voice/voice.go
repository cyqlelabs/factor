package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	"github.com/cyqlelabs/factor/internal/channel/phone"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

func init() {
	channel.Register("voice", func(raw json.RawMessage, b *bus.MessageBus) (channel.Channel, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("voice config: %w", err)
		}
		return New(cfg, b)
	})
}

const (
	// sessionKey is the one conversation this channel holds: a machine has
	// one microphone and one user.
	sessionKey = "voice:local"

	// speechReadyWait is how long the first turn may wait for the managed
	// speech server to load its models.
	speechReadyWait = 3 * time.Minute

	// pttWindow is how long an armed push-to-talk waits for the utterance.
	pttWindow = 15 * time.Second

	// ttsChunkChars keeps one synthesis call to a paragraph, so long replies
	// start sounding before they finish rendering.
	ttsChunkChars = 1200

	// echoSettleMargin is what echoSettle allows for transcription on top of
	// the silence that closes a segment.
	echoSettleMargin = 3 * time.Second

	// outboundQueue is how many bus messages may wait to be said. Deep
	// enough that a turn's notes and its reply never contend, shallow enough
	// that a channel minutes behind says so instead of hoarding.
	outboundQueue = 16
)

// Voice is the PC voice connector. Like the phone it does not publish inbound
// turns onto the bus: an utterance is synchronous — the user is standing
// there — so each one runs directly through the agent loop, and talking over
// the reply cancels the turn. The bus half carries the genuinely asynchronous
// traffic: proactive messages are spoken, and voice_write hands text to the
// user's written chat.
type Voice struct {
	cfg   Config
	home  string
	token string
	env   Env

	speech     *phone.SpeechServer // nil unless a local tier runs on Factor's own server
	speechWait time.Duration

	runner       channel.TurnFunc
	lastExternal func() (chatChannel, chatID string, ok bool)
	publish      func(bus.OutboundMessage) bool

	player *player
	utts   chan capturedUtterance
	// outbound is what the bus hands this channel, kept in the order it
	// arrived. A note from a turn still running and the reply that follows
	// it are two messages, and speaking them out of order is worse than
	// speaking neither — so Send queues here and one goroutine says them.
	outbound chan bus.OutboundMessage

	ctl    *http.Server
	ctlLn  net.Listener
	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ready atomic.Bool
	down  atomic.Value // string: why it cannot listen, "" when fine

	// Microphone gauges for the health endpoint, so "I spoke and nothing
	// happened" is answerable with one curl: the live level, the learned
	// noise floor, and whether the stream is digitally silent — the
	// signature of capturing the wrong device.
	micLevel  atomic.Uint64 // float64 bits
	micFloor  atomic.Uint64 // float64 bits
	micSilent atomic.Bool

	// speakMu keeps one voice in the room: a note from a turn still working,
	// the answer that follows it, and a proactive message all reach for the
	// same speakers, and they must queue rather than mix.
	speakMu sync.Mutex

	// echo remembers what was recently sent to the speakers, so a barged
	// utterance that is the agent's own voice off the walls is recognized
	// instead of answered.
	echo echoTracker

	// speakers is who this machine knows by voice; nil when speaker
	// identification is off. embedFailing keeps the embedding outage warning
	// to one line instead of one per utterance.
	speakers     *speakerStore
	embedFailing atomic.Bool

	// room is who is within earshot; nil when room isolation is off. It is
	// fed by every utterance the microphone resolves, including the ones the
	// activation gate turns away — somebody talking to you rather than to
	// Factor is still somebody in the room.
	room *room

	mu            sync.Mutex
	stopped       bool
	effective     Config
	client        *speechClient
	turnID        int
	turnCancel    context.CancelFunc
	pttUntil      time.Time
	windowUntil   time.Time
	lastSpeaker   string
	lastSpeakerAt time.Time
}

// New builds the connector from an already-decoded config section.
func New(cfg Config, b *bus.MessageBus) (*Voice, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("voice: %w", err)
	}
	publish := func(bus.OutboundMessage) bool { return false }
	if b != nil {
		publish = b.PublishOutbound
	}
	v := &Voice{
		cfg:        cfg,
		home:       config.Home(),
		token:      newToken(),
		env:        DefaultEnv(),
		publish:    publish,
		speechWait: speechReadyWait,
		utts:       make(chan capturedUtterance, 3),
		outbound:   make(chan bus.OutboundMessage, outboundQueue),
		effective:  cfg,
	}
	if cfg.managedSpeech() {
		v.speech = phone.NewSpeechServer(cfg.SpeechServer, v.home, cfg.Language, v.token,
			cfg.localSTT(), cfg.localTTS(), cfg.SpeakerID)
	}
	if cfg.SpeakerID {
		v.speakers = newSpeakerStore(v.home)
	}
	if cfg.roomIsolation() {
		v.room = newRoom(v.home, time.Duration(cfg.RoomTimeoutMinutes)*time.Minute)
	}
	return v, nil
}

// newToken mints the boot secret shared with the managed speech server. It
// only ever travels from Factor to its own child, through the environment.
func newToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("factor-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (v *Voice) Name() string          { return "voice" }
func (v *Voice) MaxMessageLength() int { return 0 }

// AcceptsSteering marks this channel as one whose replies also arrive through
// Send, so a spoken turn that lands on a busy session is folded into the turn
// already running rather than queued behind it. See channel.Steerable: on a
// microphone the alternative is silence the user cannot tell from a hang.
func (v *Voice) AcceptsSteering() {}

// BindTurnRunner attaches the agent loop; without it Start refuses to open
// the microphone, since nothing could answer.
func (v *Voice) BindTurnRunner(run channel.TurnFunc) { v.runner = run }

// BindLastExternal tells voice_write where the user's written conversation
// is: the terminal session in CLI mode, the last external chat in the daemon.
func (v *Voice) BindLastExternal(fn func() (string, string, bool)) { v.lastExternal = fn }

// Toolset is the tools that only exist where a microphone does.
func (v *Voice) Toolset() []tools.Tool {
	set := []tools.Tool{&writeTool{voice: v}}
	if v.speakers != nil {
		set = append(set, &speakersTool{voice: v})
	}
	if v.room != nil {
		set = append(set, &roomTool{voice: v})
	}
	return set
}

func (v *Voice) Start(ctx context.Context) error {
	if v.runner == nil {
		return fmt.Errorf("voice: the agent loop is not attached to this channel")
	}
	if _, err := captureCommand(v.env, v.cfg.InputDevice); err != nil {
		return fmt.Errorf("voice: %w", err)
	}
	playback, err := playbackCommand(v.env, v.cfg.OutputDevice)
	if err != nil {
		return fmt.Errorf("voice: %w", err)
	}
	v.player = newPlayer(v.env, playback)

	// The control endpoint is loopback-only and unauthenticated: it can arm
	// the microphone for a quarter of a minute and nothing else, and it is
	// how `factor talk` reaches a channel inside another process.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", v.cfg.ControlPort))
	if err != nil {
		return fmt.Errorf("voice: control endpoint: %w", err)
	}
	v.ctlLn = ln
	v.ctl = &http.Server{Handler: v.controlHandler(), ReadHeaderTimeout: 5 * time.Second}
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		_ = v.ctl.Serve(ln)
	}()

	v.runCtx, v.cancel = context.WithCancel(ctx)
	if v.speech != nil {
		v.speech.Start(v.runCtx)
	}
	v.wg.Add(1)
	go v.run(v.runCtx)
	v.wg.Add(1)
	go v.speakOutbound(v.runCtx)

	slog.Info("voice channel ready",
		"activation", v.cfg.Activation, "tier", v.cfg.TierLabel(), "control_port", v.cfg.ControlPort,
		"speaker_id", v.speakers != nil)
	if v.speakers != nil {
		// Who this machine already knows, and the two knobs that decide what
		// it does with a voice it does not: enough to read the decisions that
		// follow without opening the profile file.
		slog.Info("speaker identification on", "known", v.speakers.summary(),
			"threshold", v.cfg.SpeakerThreshold, "unknown_speaker", v.cfg.UnknownSpeaker)
	}
	return nil
}

func (v *Voice) Stop() error {
	v.mu.Lock()
	v.stopped = true
	v.mu.Unlock()
	if v.cancel != nil {
		v.cancel()
	}
	if v.ctl != nil {
		shutdownCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
		_ = v.ctl.Shutdown(shutdownCtx)
		done()
	}
	if v.speech != nil {
		v.speech.Stop()
	}
	if v.player != nil {
		v.player.stop()
	}
	v.wg.Wait()
	return nil
}

// Send delivers a bus message — a cron result, a finished job, a heartbeat —
// by speaking it once the channel can speak and the floor is free.
func (v *Voice) Send(_ context.Context, msg bus.OutboundMessage) error {
	// An interim arriving here belongs to a turn nobody in this process is
	// running — a finished background job reporting back, a cron result — and
	// speaking it is the only progress that turn can make audible. The notes
	// of a turn this channel runs itself never come this way; they arrive
	// from the turn (see turn), in time to be worth saying.
	if v.runCtx == nil {
		return fmt.Errorf("the voice channel never started")
	}
	if v.runCtx.Err() != nil {
		return fmt.Errorf("the voice channel is stopping")
	}
	select {
	case v.outbound <- msg:
		return nil
	default:
		// The manager retries, which is the right answer: the queue is only
		// this deep behind because the speakers are, and dropping the message
		// here would lose it silently.
		return fmt.Errorf("the voice channel has too much waiting to be said")
	}
}

// speakOutbound says what the bus handed this channel, one message at a time
// and in the order it arrived.
func (v *Voice) speakOutbound(ctx context.Context) {
	defer v.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-v.outbound:
			v.speakWhenIdle(ctx, msg.Content)
		}
	}
}

// spawn tracks a goroutine against shutdown; it refuses once Stop has begun.
func (v *Voice) spawn(f func()) bool {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		return false
	}
	v.wg.Add(1)
	v.mu.Unlock()
	go func() {
		defer v.wg.Done()
		f()
	}()
	return true
}

// run resolves the speech tier and keeps the microphone open for as long as
// the channel lives, reopening it with backoff when the helper dies.
func (v *Voice) run(ctx context.Context) {
	defer v.wg.Done()

	// The speech server loads its models first; probing earlier would demote
	// a working local tier to the cloud.
	if v.speech != nil {
		if !v.speech.WaitHealthy(ctx, v.speechWait) {
			slog.Warn("the local speech server is not ready yet", "reason", v.speech.Down())
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	effective, err := resolveAudioTier(probeCtx, v.cfg)
	cancel()
	if err != nil {
		v.setDown("%v", err)
		slog.Error("voice channel cannot listen", "error", err)
		return
	}
	v.mu.Lock()
	v.effective = effective
	v.client = newSpeechClient(effective, v.token)
	v.mu.Unlock()

	v.wg.Add(1)
	go v.utteranceWorker(ctx)

	backoff := 2 * time.Second
	for ctx.Err() == nil {
		err := v.captureLoop(ctx)
		v.ready.Store(false)
		if ctx.Err() != nil {
			return
		}
		v.setDown("microphone capture failed: %v", err)
		slog.Warn("microphone capture failed; reopening", "error", err, "backoff", backoff)
		sleepCtx(ctx, backoff)
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

// captureLoop reads frames from the capture helper and walks them through the
// segmenter. It owns the barge-in choreography: speech opening during
// playback pauses the reply on the spot, and the utterance's fate — new turn
// or resume — is decided once it has been transcribed.
func (v *Voice) captureLoop(ctx context.Context) error {
	argv, err := captureCommand(v.env, v.cfg.InputDevice)
	if err != nil {
		return err
	}
	stream, err := v.env.Capture(ctx, argv)
	if err != nil {
		return err
	}
	defer stream.Close()

	v.ready.Store(true)
	v.down.Store("")
	slog.Info("listening on the microphone", "helper", argv[0], "activation", v.cfg.Activation)

	// A live microphone always carries noise; an unbroken run of exact-zero
	// samples means the signal path is dead — usually the default source
	// being the wrong device — and that deserves saying once, not silence.
	const silentStreamFrames = 10 * 1000 / frameMs
	zeroRun, silenceWarned := 0, false

	seg := newSegmenter(v.cfg.VADRatio, v.cfg.BargeRatio, v.cfg.SilenceMs)
	frame := make([]byte, frameBytes)
	barged, overlapped := false, false
	var utterStart time.Time
	for {
		if _, err := io.ReadFull(stream, frame); err != nil {
			return err
		}
		level := rms(frame)
		v.micLevel.Store(math.Float64bits(level))
		v.micFloor.Store(math.Float64bits(seg.floor))
		if level == 0 {
			zeroRun++
		} else {
			zeroRun, silenceWarned = 0, false
		}
		v.micSilent.Store(zeroRun >= silentStreamFrames)
		if zeroRun >= silentStreamFrames && !silenceWarned {
			silenceWarned = true
			slog.Warn("the microphone is delivering pure digital silence — likely the wrong device; "+
				"list sources and set channels.voice.input_device", "helper", argv[0])
		}

		playing := v.player.playing()
		wasOpen := seg.inSpeech
		started, ended, utterance := seg.push(frame, playing)
		if playing && (wasOpen || started) {
			// The agent's voice was in the air while this segment was
			// recording. That is the wider fact than a barge and the one
			// feedback turns on: a user who starts a sentence in the pause
			// before the reply lands still has the microphone open when the
			// speakers begin, and the whole reply goes into the recording
			// behind their words.
			overlapped = true
		}
		if started {
			// The activation windows are judged against this moment, not
			// against when transcription finishes: a long sentence must not
			// outlive the window it was begun in.
			utterStart = time.Now()
		}
		if started && playing {
			// A barge-in: hold the reply while the utterance is heard out,
			// and remember the context — its transcript will carry the
			// agent's own words from the speakers alongside the user's.
			barged = true
			v.player.pause()
		}
		if !ended {
			continue
		}
		wasBarge, wasOverlap := barged, overlapped
		barged, overlapped = false, false
		if utterance == nil {
			// Too short to hold a word — a cough. Let the reply go on.
			slog.Debug("a sound opened the microphone but was too short to keep")
			v.player.resume(ctx)
			continue
		}
		slog.Debug("utterance captured", "ms", len(utterance)/2*1000/captureRate,
			"barge", wasBarge, "overlap", wasOverlap)
		select {
		case v.utts <- capturedUtterance{pcm: utterance, started: utterStart,
			barged: wasBarge, overlapped: wasOverlap}:
		default:
			slog.Warn("dropping an utterance: transcription is falling behind")
			v.player.resume(ctx)
		}
	}
}

// capturedUtterance is one segment on its way to transcription; started is
// when its first frame opened the VAD.
type capturedUtterance struct {
	pcm     []byte
	started time.Time
	// barged marks one that opened while the agent was speaking: an
	// interruption, and so a turn in its own right even without the wake word.
	barged bool
	// overlapped marks one the agent's voice was in the air for at any point
	// of, a barge included. It is what says the recording holds two voices:
	// its transcript is matched against what was spoken, and its audio is
	// never learned as anybody's — a vector mixing the user with the agent
	// belongs to neither.
	overlapped bool
}

// utteranceWorker transcribes and dispatches off the capture goroutine, so a
// slow transcription never blocks the microphone.
func (v *Voice) utteranceWorker(ctx context.Context) {
	defer v.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case utterance := <-v.utts:
			v.handleUtterance(ctx, utterance)
		}
	}
}

func (v *Voice) handleUtterance(ctx context.Context, utterance capturedUtterance) {
	text, err := v.speechClient().transcribe(ctx, utterance.pcm)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("transcription failed", "error", v.redact(err))
		}
		v.player.resume(ctx)
		return
	}
	dec := v.gate(text, utterance.started, utterance.barged, utterance.overlapped)
	var heard heardVoices
	switch {
	case dec.accept:
		heard = v.identifySpeaker(ctx, utterance)
	case v.room != nil && !dec.echo && !dec.noise:
		// Speech the gate turned away still says who is in the room. Reading
		// it costs one embedding and buys the case that matters most: a guest
		// who has been talking to the user for ten minutes is known to be
		// there before they ever address Factor, so the first private thing
		// asked after they walked in is already answered to the room.
		//
		// Speech is the word that matters: an utterance the transcriber
		// returned nothing for holds no person, and one that came back as a
		// subtitle credit holds a television. Reading either names somebody
		// out of a door slam — on this machine that has read as a voice at
		// 0.40 against the owner's own profile — and declares a room shared
		// on the strength of it. Nearly a fifth of what the detector opens
		// transcribes to nothing, so skipping it is also the cheapest
		// round-trip in the loop: the one not taken.
		heard = v.presenceOf(ctx, utterance)
	}
	who := heard.speaker
	v.room.heard(heard.present, v.speakerProfilesExist(), utterance.started)
	st := v.room.snapshot(utterance.started)
	if dec.accept {
		// Only a turn consumes the flip: the announcement is owed to somebody
		// who is listening, and the next reply is when it becomes true.
		st = v.room.assess(utterance.started)
	}
	// One line per detected utterance: what was heard and what became of it.
	// This is what turns "I spoke and nothing happened" into a diagnosis —
	// no line means the voice never reached the VAD, an empty text means the
	// transcriber filtered it, "ignored" means the gate did, and "echo" means
	// it was the agent's own voice back off the speakers.
	action := "ignored"
	switch {
	case dec.accept:
		action = "turn"
	case dec.acknowledge:
		action = "acknowledge"
	case dec.echo:
		action = "echo"
	case dec.noise && text != "":
		// Distinct from "ignored", which is the gate deciding a real sentence
		// was not addressed to Factor. This is the transcriber handing back
		// something nobody said.
		action = "noise"
	}
	fields := []any{"text", text, "action", action}
	if v.speakers != nil && (dec.accept || who.via != "") {
		fields = append(fields, who.logFields(sessionFor(who, st.Shared), dec.accept)...)
	}
	// How many distinct people were in this one recording. Worth a field only
	// when it is more than one, which is the case the split exists for and
	// the case a reader is trying to confirm.
	if len(heard.present) > 1 {
		fields = append(fields, "voices", len(heard.present))
	}
	if v.room != nil {
		fields = append(fields, "room", st.label(), "present", strings.Join(st.Present, ", "))
	}
	slog.Info("voice heard", fields...)
	switch {
	case dec.accept:
		// The new utterance owns the floor: whatever was playing is over,
		// and a turn still thinking answers a question nobody is waiting on.
		v.player.stop()
		v.cancelTurn()
		v.spawn(func() { v.turn(ctx, dec.text, who, st) })
	case dec.acknowledge:
		v.player.stop()
		v.spawn(func() { v.speak(ctx, ackLine(v.cfg.Language)) })
	default:
		v.player.resume(ctx)
	}
}

// decision is what the activation gate makes of one transcribed utterance.
type decision struct {
	accept      bool
	acknowledge bool // the bare wake word: attention, not a request
	echo        bool // the agent's own voice off the speakers, nothing else
	noise       bool // nothing a person said: no transcript, or a subtitle credit
	text        string
}

// gate decides an utterance's fate. started is when the user began the
// utterance — the windows are held against that moment, because the seconds
// the sentence itself and its transcription take are not the user hesitating.
// barged marks one that opened over the agent's own voice: speech deliberate
// enough to clear the raised barge thresholds is an interruption in its own
// right, so it is accepted without the wake word. overlapped is the wider
// fact — the agent was speaking at some point while this was recording,
// whether or not it had started yet — and it is what decides whether the
// transcript has to be cleaned of the agent's own words.
func (v *Voice) gate(text string, started time.Time, barged, overlapped bool) decision {
	text = strings.TrimSpace(text)
	if text == "" || noiseOnly(text) {
		return decision{noise: true}
	}
	if overlapped {
		// The speakers' own sound is in this recording. At high volume it
		// survives transcription, and an utterance that is nothing but the
		// agent's recent words is feedback — answering it is the agent
		// talking to itself.
		rest, echoOnly := v.echo.strip(text)
		if echoOnly || rest == "" {
			return decision{echo: true}
		}
		text = rest
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// An armed push-to-talk admits the next utterance in any mode: it is the
	// rescue for a wake word that misfires.
	if started.Before(v.pttUntil) {
		v.pttUntil = time.Time{}
		return decision{accept: true, text: text}
	}
	switch v.cfg.Activation {
	case activationPTT:
		return decision{}
	case activationWakeWord:
		if stripped, ok := stripWakeWord(text, v.cfg.WakeWord, overlapped); ok {
			if stripped == "" {
				v.windowUntil = time.Now().Add(v.followUp())
				return decision{acknowledge: true}
			}
			return decision{accept: true, text: stripped}
		}
		if barged {
			// Talking over the reply is the interruption; the wake word
			// cannot be required of it, because it is spoken over the agent's
			// own voice — the one word transcription mangles most.
			return decision{accept: true, text: text}
		}
		if started.Before(v.windowUntil) {
			return decision{accept: true, text: text}
		}
		return decision{}
	default:
		return decision{accept: true, text: text}
	}
}

func (v *Voice) followUp() time.Duration {
	return time.Duration(v.cfg.FollowUpSeconds) * time.Second
}

// turn runs one utterance through the agent loop and speaks the reply.
func (v *Voice) turn(parent context.Context, text string, who speakerIdentity, st roomState) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	v.mu.Lock()
	v.turnID++
	id := v.turnID
	v.turnCancel = cancel
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		if v.turnID == id {
			v.turnCancel = nil
		}
		v.mu.Unlock()
	}()

	// What the agent says before a tool call is spoken as it happens, on its
	// own goroutine: the point of a filler line is to fill the time the tools
	// take, not to be queued behind them. speak serialises it against the
	// answer, so the two arrive in the order they were written.
	notice := func(line string) {
		v.spawn(func() { v.speak(ctx, line) })
	}
	// The room's flip is said out loud before the answer that depends on it,
	// so a user who has just been made discreet knows why — a silent switch
	// is untrustworthy in both directions, and the one that goes quiet about
	// the user's own calendar reads as a fault rather than a courtesy.
	if st.Changed {
		v.speak(ctx, roomChangeLine(st, v.cfg.Language))
	}
	reply, err := v.runner(ctx, text, sessionFor(who, st.Shared), who.attributed(), st.audience(), notice)
	if ctx.Err() != nil {
		return // barged in on — the next utterance owns the conversation
	}
	if err != nil {
		slog.Error("voice turn failed", "error", err)
		reply = spokenFailure(v.cfg.Language)
	}
	if strings.TrimSpace(reply) == "" {
		return
	}
	v.speak(ctx, reply)
}

// speak synthesises and plays text, a paragraph at a time, returning once the
// last chunk has been heard or the floor was taken. The wake-word follow-up
// window opens as it ends, so answering back does not need the wake word.
func (v *Voice) speak(ctx context.Context, text string) {
	client := v.speechClient()
	if client == nil {
		return
	}
	v.speakMu.Lock()
	defer v.speakMu.Unlock()
	if ctx.Err() != nil {
		return // the floor was taken while this waited its turn
	}
	defer v.armWindow()
	// However this reply ends — heard out, or cut off by a barge partway —
	// what reached the speakers settles on the same delay. See echoSettle.
	defer v.echo.expire(v.echoSettle())
	text = spokenText(text, v.cfg.Language)
	if text == "" {
		return
	}
	for _, chunk := range channel.SplitMessage(text, ttsChunkChars) {
		pcm, err := client.synthesize(ctx, chunk)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("speech synthesis failed", "error", v.redact(err))
			}
			return
		}
		if len(pcm) == 0 {
			continue
		}
		scalePCM(pcm, v.cfg.OutputVolume)
		v.echo.record(chunk)
		done := v.player.play(ctx, pcm)
		select {
		case <-ctx.Done():
			return
		case result := <-done:
			if !result.completed {
				return
			}
		}
	}
}

// echoSettle is how long a reply that has stopped sounding stays matchable as
// echo. The segment that captured the tail of it does not close until
// silence_ms of quiet has passed, and is transcribed only after that:
// forgetting the words the moment the speakers fall silent throws them away a
// beat before the echo carrying them arrives. Past the delay they are
// forgotten, so a user who quotes the reply later is not swallowed.
func (v *Voice) echoSettle() time.Duration {
	return time.Duration(v.cfg.SilenceMs)*time.Millisecond + echoSettleMargin
}

func (v *Voice) armWindow() {
	v.mu.Lock()
	v.windowUntil = time.Now().Add(v.followUp())
	v.mu.Unlock()
}

// speakWhenIdle waits for the channel to be able to speak, then for the floor,
// before a proactive message: it must not talk over a reply, or over the
// user's turn in progress. The startup wait matters most to the restart
// notice, which the gateway delivers the moment it boots — while the local
// speech server is still loading its models.
func (v *Voice) speakWhenIdle(ctx context.Context, text string) {
	started := time.Now()
	for v.speechClient() == nil {
		if ctx.Err() != nil {
			return
		}
		// run settles the tier within speechWait plus one probe, one way or
		// the other; past that the channel is down and has said so.
		if time.Since(started) > v.speechWait+30*time.Second {
			slog.Warn("dropping a spoken message: the speech tier never came up", "text", text)
			return
		}
		sleepCtx(ctx, 500*time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		if !v.player.busy() && !v.turnInFlight() {
			v.speak(ctx, text)
			return
		}
		sleepCtx(ctx, 500*time.Millisecond)
	}
	if ctx.Err() == nil {
		slog.Warn("dropping a spoken message: the floor was never free to say it", "text", text)
	}
}

func (v *Voice) turnInFlight() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.turnCancel != nil
}

func (v *Voice) cancelTurn() {
	v.mu.Lock()
	cancel := v.turnCancel
	v.turnCancel = nil
	v.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ArmPTT arms push-to-talk: it admits the next utterance unconditionally. The reply being spoken is
// abandoned: the user pressed the button to say something else.
func (v *Voice) ArmPTT() {
	if v.player != nil {
		v.player.stop()
	}
	v.mu.Lock()
	v.pttUntil = time.Now().Add(pttWindow)
	v.mu.Unlock()
	slog.Info("push-to-talk armed", "window", pttWindow)
}

func (v *Voice) speechClient() *speechClient {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.client
}

func (v *Voice) setDown(format string, args ...any) {
	v.down.Store(fmt.Sprintf(format, args...))
}

// controlHandler is the loopback API `factor talk` and `factor status` use.
func (v *Voice) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		status := "starting"
		if v.ready.Load() {
			status = "ok"
		}
		reason, _ := v.down.Load().(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     status,
			"reason":     reason,
			"activation": v.cfg.Activation,
			"tier":       v.tierLabel(),
			// The microphone gauges: what the mic delivers right now, the
			// noise floor the VAD learned, and whether the stream is
			// digitally silent (the wrong-device signature).
			"mic_level":  math.Round(math.Float64frombits(v.micLevel.Load())),
			"mic_floor":  math.Round(math.Float64frombits(v.micFloor.Load())),
			"mic_silent": v.micSilent.Load(),
			// Who speaker identification last heard; "" when off or unknown.
			"last_speaker": v.lastSpeakerName(),
			// Who is believed within earshot, and so whether replies and
			// recall are being scoped to a shared room. "off" when the
			// feature is not configured, so the endpoint distinguishes an
			// empty room from one nothing is watching.
			"room":         v.roomLabel(),
			"room_present": v.roomPresent(),
		})
	})
	mux.HandleFunc("POST /ptt", func(w http.ResponseWriter, _ *http.Request) {
		v.ArmPTT()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// roomLabel and roomPresent report the room for status readouts, without
// consuming the transition announcement the next turn owes the user.
func (v *Voice) roomLabel() string {
	if v.room == nil {
		return "off"
	}
	return v.room.snapshot(time.Now()).label()
}

func (v *Voice) roomPresent() []string {
	return v.room.snapshot(time.Now()).Present
}

func (v *Voice) tierLabel() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.effective.TierLabel()
}

// Meter is a live reading of the channel's ears and mouth, for status UIs.
type Meter struct {
	Ready    bool    // the microphone is open
	Level    float64 // what the mic delivers right now (RMS)
	Floor    float64 // the noise floor the VAD learned
	Silent   bool    // digital silence: the wrong-device signature
	Speaking bool    // sound is coming out of the speakers
}

// Meter reports the channel's live state; safe from any goroutine.
func (v *Voice) Meter() Meter {
	m := Meter{
		Ready:  v.ready.Load(),
		Level:  math.Float64frombits(v.micLevel.Load()),
		Floor:  math.Float64frombits(v.micFloor.Load()),
		Silent: v.micSilent.Load(),
	}
	if v.player != nil {
		m.Speaking = v.player.playing()
	}
	return m
}

// redact keeps speech credentials out of anything a user or the model sees.
func (v *Voice) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, secret := range []string{v.cfg.STTAPIKey, v.cfg.ElevenLabsAPIKey, v.token} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return fmt.Errorf("%s", msg)
}

// stripWakeWord reports whether text carries the wake word and returns what
// follows it. Normally it must open the utterance — one filler word allowed,
// so "hey factor" works. With anywhere, it counts at any position: a barge-in
// transcript opens with whatever the speakers were saying when the user
// talked over them.
func stripWakeWord(text, wake string, anywhere bool) (string, bool) {
	var wakeWords []string
	for _, w := range strings.Fields(wake) {
		if n := normalizeWord(w); n != "" {
			wakeWords = append(wakeWords, n)
		}
	}
	if len(wakeWords) == 0 {
		return "", false
	}
	tokens := tokenizeWords(text)

	maxPos := 1
	if anywhere {
		maxPos = len(tokens)
	}
	for pos := 0; pos <= maxPos && pos+len(wakeWords) <= len(tokens); pos++ {
		match := true
		for j, w := range wakeWords {
			if tokens[pos+j].norm != w {
				match = false
				break
			}
		}
		if match {
			rest := text[tokens[pos+len(wakeWords)-1].end:]
			rest = strings.TrimLeft(rest, " \t\n,.;:!?¡¿-—")
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// normalizeWord lowers a word and strips everything that is not a letter or
// digit, so "Factor," matches "factor".
func normalizeWord(w string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(w) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func spokenFailure(language string) string {
	if isSpanish(language) {
		return "Perdón — algo salió mal aquí. ¿Me lo repites?"
	}
	return "Sorry — something went wrong on my end. Could you say that again?"
}

func ackLine(language string) string {
	if isSpanish(language) {
		return "¿Sí?"
	}
	return "Yes?"
}

func isSpanish(language string) bool {
	return strings.HasPrefix(strings.ToLower(language), "es")
}

// sleepCtx waits out d, or returns the moment ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
