package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/session"
	"github.com/cyqlelabs/factor/internal/tools"
)

// ChatProvider is what the loop needs from the provider layer (satisfied by
// *provider.Chain; tests use scripted fakes).
type ChatProvider interface {
	Chat(ctx context.Context, req *provider.Request) (*provider.Response, error)
}

const steeringBuffer = 8

// turn tracks one in-flight session turn; extra inbound messages for the
// same session land in the steering queue and are injected between
// iterations instead of queuing a second turn.
type turn struct {
	steering chan bus.InboundMessage
}

type Loop struct {
	cfg      *config.Config
	bus      *bus.MessageBus
	chat     ChatProvider
	registry *tools.Registry
	sessions *session.Store
	builder  *ContextBuilder
	ambient  *memory.Ambient

	mu        sync.Mutex
	active    map[string]*turn
	sem       chan struct{}
	wg        sync.WaitGroup
	compactMu sync.Mutex

	watchMu sync.RWMutex
	watcher func(Activity)

	lastMu      sync.Mutex
	lastChannel bus.InboundMessage
	reachable   func(channel string) bool
}

func NewLoop(cfg *config.Config, b *bus.MessageBus, chat ChatProvider, registry *tools.Registry,
	sessions *session.Store, builder *ContextBuilder, ambient *memory.Ambient) *Loop {
	return &Loop{
		cfg:         cfg,
		bus:         b,
		chat:        chat,
		registry:    registry,
		sessions:    sessions,
		builder:     builder,
		ambient:     ambient,
		active:      map[string]*turn{},
		sem:         make(chan struct{}, cfg.Agent.MaxConcurrentTurns),
		lastChannel: loadLastChannel(),
	}
}

// Run drains the inbound bus until ctx is cancelled. One live turn per
// session key; overflow becomes steering.
func (l *Loop) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return
		case msg := <-l.bus.Inbound():
			l.dispatch(ctx, msg)
		}
	}
}

// claim registers a live turn for key, or steers msg into the existing one.
// The steering send happens under l.mu — the same lock release drains under —
// so a message can never slip into a turn that has already been released.
func (l *Loop) claim(key string, steer *bus.InboundMessage) (*turn, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, running := l.active[key]; running {
		if steer != nil {
			select {
			case t.steering <- *steer:
				slog.Info("steering message into live turn", "session", key)
			default:
				slog.Warn("steering queue full, dropping message", "session", key)
			}
		}
		return nil, false
	}
	t := &turn{steering: make(chan bus.InboundMessage, steeringBuffer)}
	l.active[key] = t
	return t, true
}

// release removes the claim and returns any steering messages that arrived
// too late for the finished turn, so the caller can republish them as fresh
// inbound work instead of losing them.
func (l *Loop) release(key string, t *turn) []bus.InboundMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.active, key)
	var leftover []bus.InboundMessage
	for {
		select {
		case msg := <-t.steering:
			leftover = append(leftover, msg)
		default:
			return leftover
		}
	}
}

func (l *Loop) dispatch(ctx context.Context, msg bus.InboundMessage) {
	key := msg.SessionKey()
	t, ok := l.claim(key, &msg)
	if !ok {
		return
	}

	l.recordLastChannel(msg)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			for _, missed := range l.release(key, t) {
				l.bus.PublishInbound(missed)
			}
		}()
		select {
		case l.sem <- struct{}{}:
			defer func() { <-l.sem }()
		case <-ctx.Done():
			return
		}

		reply, err := l.runTurn(ctx, msg, t, nil)
		if err != nil {
			slog.Error("turn failed", "session", key, "error", err)
			reply = fmt.Sprintf("Something went wrong: %v", err)
		}
		if strings.TrimSpace(reply) != "" {
			l.bus.PublishOutbound(bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Content: reply})
		}
	}()
}

// ProcessDirect runs a synchronous turn outside the bus (CLI one-shot,
// cron, delegated jobs) with nobody listening for progress.
func (l *Loop) ProcessDirect(ctx context.Context, content, sessionKey string) (string, error) {
	return l.ProcessDirectNotice(ctx, content, sessionKey, nil)
}

// ProcessDirectNotice runs a synchronous turn outside the bus and reports
// what the agent says on its way to the answer to notice, as it says it —
// the connector running the turn is the only place that line can arrive
// while it is still news. It honors the one-live-turn-per-session invariant:
// if the session is busy (e.g. an overlapping cron firing), it waits for
// the claim instead of interleaving histories.
func (l *Loop) ProcessDirectNotice(ctx context.Context, content, sessionKey string,
	notice func(string)) (string, error) {
	msg := bus.InboundMessage{Channel: "cli", ChatID: strings.TrimPrefix(sessionKey, "cli:"), Content: content}
	if idx := strings.IndexByte(sessionKey, ':'); idx > 0 {
		msg.Channel, msg.ChatID = sessionKey[:idx], sessionKey[idx+1:]
	}
	var t *turn
	for {
		var ok bool
		if t, ok = l.claim(sessionKey, nil); ok {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer func() {
		for _, missed := range l.release(sessionKey, t) {
			l.bus.PublishInbound(missed)
		}
	}()
	return l.runTurn(ctx, msg, t, notice)
}

// ProcessEphemeral runs a history-less, memory-less turn (heartbeat).
func (l *Loop) ProcessEphemeral(ctx context.Context, content string) (string, error) {
	return l.execute(ctx, turnInput{
		sessionKey: "system:heartbeat",
		content:    content,
		ephemeral:  true,
		toolCtx:    tools.ToolContext{Channel: "system", ChatID: "heartbeat", SessionKey: "system:heartbeat"},
	}, &turn{steering: make(chan bus.InboundMessage, 1)})
}

type turnInput struct {
	sessionKey string
	content    string
	ephemeral  bool
	// notice hands the line the agent says on its way to an answer straight
	// back to whoever is running this turn, while it is still true. Nil for
	// turns nobody is waiting on synchronously; those are watched instead.
	notice  func(string)
	toolCtx tools.ToolContext
}

func (l *Loop) runTurn(ctx context.Context, msg bus.InboundMessage, t *turn, notice func(string)) (string, error) {
	reply, err := l.execute(ctx, turnInput{
		sessionKey: msg.SessionKey(),
		content:    msg.Content,
		notice:     notice,
		toolCtx:    tools.ToolContext{Channel: msg.Channel, ChatID: msg.ChatID, SessionKey: msg.SessionKey()},
	}, t)
	if err == nil && l.ambient != nil {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.ambient.StoreExchange(msg.Channel, msg.Content, reply)
		}()
	}
	return reply, err
}

// Idle reports whether every session has finished its turn. The gateway
// asks before restarting itself: a reload mid-turn is an answer the user
// never gets.
func (l *Loop) Idle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.active) == 0
}

// WaitBackground blocks until async work (memory stores, compaction) drains,
// or the timeout passes. One-shot mode calls this so memory writes are not
// lost to process exit.
func (l *Loop) WaitBackground(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// execute is the turn state machine: assemble → chat → tools → steer → repeat.
func (l *Loop) execute(ctx context.Context, in turnInput, t *turn) (string, error) {
	ctx = tools.WithToolContext(ctx, in.toolCtx)
	l.emit(in.sessionKey, PhaseContext, "")
	defer l.emit(in.sessionKey, PhaseDone, "")

	var history []provider.Message
	var err error
	if !in.ephemeral {
		if history, err = l.sessions.History(in.sessionKey); err != nil {
			return "", fmt.Errorf("load history: %w", err)
		}
	}

	systemPrompt := l.builder.SystemPrompt(ctx, history, in.content)
	userMsg := provider.Message{Role: "user", Content: in.content}
	if err := l.persist(in, userMsg); err != nil {
		return "", err
	}

	messages := l.assemble(systemPrompt, in.sessionKey, in.ephemeral, append(history, userMsg))
	overflowRecoveries := 0

	for iteration := 0; iteration < l.cfg.Agent.MaxToolIterations; iteration++ {
		l.emit(in.sessionKey, PhaseThinking, "")
		resp, err := l.chat.Chat(ctx, &provider.Request{
			Messages:    messages,
			Tools:       l.registry.Definitions(),
			MaxTokens:   l.cfg.Provider.MaxTokens,
			Temperature: l.cfg.Provider.Temperature,
		})
		if err != nil {
			if provider.IsContextOverflow(err) && !in.ephemeral && overflowRecoveries < 2 {
				overflowRecoveries++
				l.emit(in.sessionKey, PhaseCompacting, "")
				slog.Warn("context overflow; compacting session", "session", in.sessionKey)
				if cerr := l.compact(ctx, in.sessionKey); cerr != nil {
					return "", fmt.Errorf("compact after overflow: %w (original: %v)", cerr, err)
				}
				fresh, herr := l.sessions.History(in.sessionKey)
				if herr != nil {
					return "", herr
				}
				messages = l.assemble(systemPrompt, in.sessionKey, in.ephemeral, fresh)
				iteration--
				continue
			}
			return "", err
		}

		assistantMsg := provider.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls}
		if err := l.persist(in, assistantMsg); err != nil {
			return "", err
		}
		messages = append(messages, assistantMsg)

		if len(resp.ToolCalls) == 0 {
			// Final answer — unless steering arrived mid-turn; then keep going.
			steered := l.drainSteering(in, t)
			if len(steered) == 0 {
				l.maybeCompactAsync(in)
				return resp.Content, nil
			}
			messages = append(messages, steered...)
			continue
		}

		// Text alongside tool calls is the agent saying what it is about to
		// do. It reaches the user now, while the tools run, instead of after
		// the whole turn — on a chat channel that is the difference between a
		// silent minute and a conversation.
		if notice := strings.TrimSpace(resp.Content); notice != "" && !in.ephemeral {
			l.emit(in.sessionKey, PhaseNotice, notice)
			if in.notice != nil {
				in.notice(notice)
			}
		}

		for _, call := range resp.ToolCalls {
			l.emit(in.sessionKey, PhaseTool, call.Name)
			result := l.registry.Execute(ctx, call.Name, call.Args)
			content := result.ForLLM
			if result.IsError {
				content = "ERROR: " + content
			}
			toolMsg := provider.Message{Role: "tool", ToolCallID: call.ID, Content: content}
			if err := l.persist(in, toolMsg); err != nil {
				return "", err
			}
			messages = append(messages, toolMsg)

			// A tool result carrying images (screen_view's annotated
			// screenshot) rides a follow-up user message — the one placement
			// every vision dialect accepts. Only the placeholder is persisted:
			// session files stay small, replay stays valid on any model, and
			// a stale frame is worthless next turn anyway.
			if len(result.Images) > 0 {
				if err := l.persist(in, provider.Message{Role: "user", Content: imagePruned(call.Name)}); err != nil {
					return "", err
				}
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: fmt.Sprintf("[Attached: image output of the %s tool. This is tool output, not a message from the user.]", call.Name),
					Images:  result.Images,
				})
				pruneImages(messages)
			}
		}
		messages = append(messages, l.drainSteering(in, t)...)
	}

	reply := l.wrapUp(ctx, in, messages)
	l.maybeCompactAsync(in)
	return reply, nil
}

// wrapUp buys back a turn that ran out of tool iterations. Those iterations
// hold real findings, and spending them to arrive at an apology is the worst
// answer available: one more pass with the tools withheld turns the work into
// something the user can act on. If even that fails, say what happened.
func (l *Loop) wrapUp(ctx context.Context, in turnInput, messages []provider.Message) string {
	const stalled = "I hit the tool-iteration limit for this turn without reaching a final answer. Ask me to continue if you want me to keep going."
	slog.Warn("tool-iteration limit reached; asking for a final answer with no tools", "session", in.sessionKey)

	nudge := provider.Message{Role: "user", Content: "You have used every tool iteration this turn allows, so no further tool calls are possible. " +
		"Answer now from what you already gathered: your best conclusion, what is still unverified, and the next step you would take."}
	if err := l.persist(in, nudge); err != nil {
		return stalled
	}
	l.emit(in.sessionKey, PhaseThinking, "")
	resp, err := l.chat.Chat(ctx, &provider.Request{
		Messages:    append(messages, nudge),
		MaxTokens:   l.cfg.Provider.MaxTokens,
		Temperature: l.cfg.Provider.Temperature,
	})
	if err != nil {
		slog.Error("wrap-up call failed", "session", in.sessionKey, "error", err)
		return stalled
	}
	if strings.TrimSpace(resp.Content) == "" {
		return stalled
	}
	if err := l.persist(in, provider.Message{Role: "assistant", Content: resp.Content}); err != nil {
		slog.Error("persist wrap-up answer", "session", in.sessionKey, "error", err)
	}
	return resp.Content
}

// maxImagesInContext bounds how many attached images ride one in-flight turn.
// Screenshots are the token heavyweight of a desktop session; only the newest
// frames still inform the next action, so older ones collapse to a note.
const maxImagesInContext = 2

func imagePruned(toolName string) string {
	return fmt.Sprintf("[An image from the %s tool was attached here and has been dropped to save space. Re-run the tool for a fresh view.]", toolName)
}

// pruneImages strips image payloads from all but the newest
// maxImagesInContext image-bearing messages, in place.
func pruneImages(messages []provider.Message) {
	withImages := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if len(messages[i].Images) == 0 {
			continue
		}
		withImages++
		if withImages <= maxImagesInContext {
			continue
		}
		name := "screen_view"
		if _, after, ok := strings.Cut(messages[i].Content, "image output of the "); ok {
			name, _, _ = strings.Cut(after, " ")
		}
		messages[i].Images = nil
		messages[i].Content = imagePruned(name)
	}
}

// assemble builds the request message list: system, optional summary, history.
func (l *Loop) assemble(systemPrompt, sessionKey string, ephemeral bool, history []provider.Message) []provider.Message {
	messages := []provider.Message{{Role: "system", Content: systemPrompt}}
	if !ephemeral {
		if summary := l.sessions.Summary(sessionKey); summary != "" {
			messages = append(messages, provider.Message{Role: "system", Content: "Summary of earlier conversation:\n" + summary})
		}
	}
	return append(messages, history...)
}

func (l *Loop) persist(in turnInput, msg provider.Message) error {
	if in.ephemeral {
		return nil
	}
	return l.sessions.Append(in.sessionKey, msg)
}

func (l *Loop) drainSteering(in turnInput, t *turn) []provider.Message {
	var out []provider.Message
	for {
		select {
		case msg := <-t.steering:
			userMsg := provider.Message{Role: "user", Content: msg.Content}
			if err := l.persist(in, userMsg); err != nil {
				slog.Error("persist steering message", "error", err)
			}
			out = append(out, userMsg)
		default:
			if len(out) > 0 {
				l.emit(in.sessionKey, PhaseSteering, "")
			}
			return out
		}
	}
}

func (l *Loop) recordLastChannel(msg bus.InboundMessage) {
	if !bus.External(msg.Channel) {
		return
	}
	l.lastMu.Lock()
	moved := l.lastChannel.Channel != msg.Channel || l.lastChannel.ChatID != msg.ChatID
	l.lastChannel = msg
	l.lastMu.Unlock()
	if moved {
		saveLastChannel(msg)
	}
}

// SetReachable teaches the loop which channels have a connector behind them
// right now, so LastChannel never hands out an address nothing can deliver
// to. Unset — the CLI, tests — every channel counts as reachable.
func (l *Loop) SetReachable(fn func(channel string) bool) {
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	l.reachable = fn
}

// LastChannel returns the most recent external channel/chat, for heartbeat
// and cron delivery. It survives a restart — the address is on disk — so
// ok is false until the user's first ever message, and again whenever that
// chat's connector is not running: an address whose channel was switched off
// is not somewhere Factor can reach anyone, and a caller that treats it as
// one reports a delivery that never happened.
func (l *Loop) LastChannel() (channel, chatID string, ok bool) {
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	if l.lastChannel.Channel == "" {
		return "", "", false
	}
	if l.reachable != nil && !l.reachable(l.lastChannel.Channel) {
		return "", "", false
	}
	return l.lastChannel.Channel, l.lastChannel.ChatID, true
}
