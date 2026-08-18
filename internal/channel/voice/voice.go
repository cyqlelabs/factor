package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
)

// voiceBrief opens the first turn of a boot, so the agent knows how its words
// will arrive. It is the same trick the phone bridge uses for outbound calls.
const voiceBrief = "[Heard on this machine's microphone; your reply will be spoken aloud. " +
	"Keep spoken replies short and conversational — no markdown, no lists. When the user asks " +
	"for a written reply, or the content would be painful to listen to (code, lists, links, " +
	"anything long), deliver it with the voice_write tool and keep the spoken reply to a sentence.]"

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
	utts   chan []byte

	ctl    *http.Server
	ctlLn  net.Listener
	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ready atomic.Bool
	down  atomic.Value // string: why it cannot listen, "" when fine

	mu          sync.Mutex
	stopped     bool
	effective   Config
	client      *speechClient
	turnID      int
	turnCancel  context.CancelFunc
	pttUntil    time.Time
	windowUntil time.Time
	briefed     bool
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
		utts:       make(chan []byte, 3),
		effective:  cfg,
	}
	if cfg.managedSpeech() {
		v.speech = phone.NewSpeechServer(cfg.SpeechServer, v.home, cfg.Language, v.token,
			cfg.localSTT(), cfg.localTTS())
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

// BindTurnRunner attaches the agent loop; without it Start refuses to open
// the microphone, since nothing could answer.
func (v *Voice) BindTurnRunner(run channel.TurnFunc) { v.runner = run }

// BindLastExternal tells voice_write where the user's written conversation
// is: the terminal session in CLI mode, the last external chat in the daemon.
func (v *Voice) BindLastExternal(fn func() (string, string, bool)) { v.lastExternal = fn }

// Toolset is the tool that only exists where a microphone does.
func (v *Voice) Toolset() []tools.Tool { return []tools.Tool{&writeTool{voice: v}} }

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

	slog.Info("voice channel ready",
		"activation", v.cfg.Activation, "tier", v.cfg.TierLabel(), "control_port", v.cfg.ControlPort)
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
// by speaking it once the floor is free.
func (v *Voice) Send(_ context.Context, msg bus.OutboundMessage) error {
	// A note about work in progress is worth a chat bubble, not a voice
	// interrupting the room.
	if msg.Interim {
		return nil
	}
	if v.speechClient() == nil {
		return fmt.Errorf("the voice channel is still starting")
	}
	if !v.spawn(func() { v.speakWhenIdle(v.runCtx, msg.Content) }) {
		return fmt.Errorf("the voice channel is stopping")
	}
	return nil
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

	seg := newSegmenter(v.cfg.VADRatio, v.cfg.BargeRatio, v.cfg.SilenceMs)
	frame := make([]byte, frameBytes)
	for {
		if _, err := io.ReadFull(stream, frame); err != nil {
			return err
		}
		playing := v.player.playing()
		started, ended, utterance := seg.push(frame, playing)
		if started && playing {
			v.player.pause()
		}
		if !ended {
			continue
		}
		if utterance == nil {
			// Too short to hold a word — a cough. Let the reply go on.
			v.player.resume(ctx)
			continue
		}
		select {
		case v.utts <- utterance:
		default:
			slog.Warn("dropping an utterance: transcription is falling behind")
			v.player.resume(ctx)
		}
	}
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

func (v *Voice) handleUtterance(ctx context.Context, pcm []byte) {
	text, err := v.speechClient().transcribe(ctx, pcm)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("transcription failed", "error", v.redact(err))
		}
		v.player.resume(ctx)
		return
	}
	dec := v.gate(text)
	switch {
	case dec.accept:
		// The new utterance owns the floor: whatever was playing is over,
		// and a turn still thinking answers a question nobody is waiting on.
		v.player.stop()
		v.cancelTurn()
		v.spawn(func() { v.turn(ctx, dec.text) })
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
	text        string
}

func (v *Voice) gate(text string) decision {
	text = strings.TrimSpace(text)
	if text == "" {
		return decision{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	// An armed push-to-talk admits the next utterance in any mode: it is the
	// rescue for a wake word that misfires.
	if now.Before(v.pttUntil) {
		v.pttUntil = time.Time{}
		return decision{accept: true, text: text}
	}
	switch v.cfg.Activation {
	case activationPTT:
		return decision{}
	case activationWakeWord:
		if stripped, ok := stripWakeWord(text, v.cfg.WakeWord); ok {
			if stripped == "" {
				v.windowUntil = now.Add(v.followUp())
				return decision{acknowledge: true}
			}
			return decision{accept: true, text: stripped}
		}
		if now.Before(v.windowUntil) {
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
func (v *Voice) turn(parent context.Context, text string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	v.mu.Lock()
	v.turnID++
	id := v.turnID
	v.turnCancel = cancel
	content := text
	if !v.briefed {
		v.briefed = true
		content = voiceBrief + "\n\n" + text
	}
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		if v.turnID == id {
			v.turnCancel = nil
		}
		v.mu.Unlock()
	}()

	reply, err := v.runner(ctx, content, sessionKey)
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
	defer v.armWindow()
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

func (v *Voice) armWindow() {
	v.mu.Lock()
	v.windowUntil = time.Now().Add(v.followUp())
	v.mu.Unlock()
}

// speakWhenIdle waits for the floor before a proactive message: it must not
// talk over a reply, or over the user's turn in progress.
func (v *Voice) speakWhenIdle(ctx context.Context, text string) {
	deadline := time.Now().Add(2 * time.Minute)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		if !v.player.busy() && !v.turnInFlight() {
			v.speak(ctx, text)
			return
		}
		sleepCtx(ctx, 500*time.Millisecond)
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

// armPTT admits the next utterance unconditionally. The reply being spoken is
// abandoned: the user pressed the button to say something else.
func (v *Voice) armPTT() {
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
		})
	})
	mux.HandleFunc("POST /ptt", func(w http.ResponseWriter, _ *http.Request) {
		v.armPTT()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (v *Voice) tierLabel() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.effective.TierLabel()
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

// stripWakeWord reports whether text opens with the wake word — allowing one
// filler word before it, so "hey factor" works — and returns what follows.
func stripWakeWord(text, wake string) (string, bool) {
	var wakeWords []string
	for _, w := range strings.Fields(wake) {
		if n := normalizeWord(w); n != "" {
			wakeWords = append(wakeWords, n)
		}
	}
	if len(wakeWords) == 0 {
		return "", false
	}

	type token struct {
		norm string
		end  int // byte offset just past the word in the original text
	}
	var tokens []token
	start := -1
	for i, r := range text {
		if r == ' ' || r == '\t' || r == '\n' {
			if start >= 0 {
				tokens = append(tokens, token{normalizeWord(text[start:i]), i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		tokens = append(tokens, token{normalizeWord(text[start:]), len(text)})
	}

	for pos := 0; pos <= 1 && pos+len(wakeWords) <= len(tokens); pos++ {
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
