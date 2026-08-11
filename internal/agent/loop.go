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

	mu     sync.Mutex
	active map[string]*turn
	sem    chan struct{}
	wg     sync.WaitGroup

	lastMu      sync.Mutex
	lastChannel bus.InboundMessage
}

func NewLoop(cfg *config.Config, b *bus.MessageBus, chat ChatProvider, registry *tools.Registry,
	sessions *session.Store, builder *ContextBuilder, ambient *memory.Ambient) *Loop {
	return &Loop{
		cfg:      cfg,
		bus:      b,
		chat:     chat,
		registry: registry,
		sessions: sessions,
		builder:  builder,
		ambient:  ambient,
		active:   map[string]*turn{},
		sem:      make(chan struct{}, cfg.Agent.MaxConcurrentTurns),
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

func (l *Loop) dispatch(ctx context.Context, msg bus.InboundMessage) {
	key := msg.SessionKey()
	l.mu.Lock()
	if t, running := l.active[key]; running {
		l.mu.Unlock()
		select {
		case t.steering <- msg:
			slog.Info("steering message into live turn", "session", key)
		default:
			slog.Warn("steering queue full, dropping message", "session", key)
		}
		return
	}
	t := &turn{steering: make(chan bus.InboundMessage, steeringBuffer)}
	l.active[key] = t
	l.mu.Unlock()

	l.recordLastChannel(msg)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			delete(l.active, key)
			l.mu.Unlock()
		}()
		select {
		case l.sem <- struct{}{}:
			defer func() { <-l.sem }()
		case <-ctx.Done():
			return
		}

		reply, err := l.runTurn(ctx, msg, t)
		if err != nil {
			slog.Error("turn failed", "session", key, "error", err)
			reply = fmt.Sprintf("Something went wrong: %v", err)
		}
		if strings.TrimSpace(reply) != "" {
			l.bus.PublishOutbound(bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Content: reply})
		}
	}()
}

// ProcessDirect runs a synchronous turn outside the bus (CLI, cron).
func (l *Loop) ProcessDirect(ctx context.Context, content, sessionKey string) (string, error) {
	msg := bus.InboundMessage{Channel: "cli", ChatID: strings.TrimPrefix(sessionKey, "cli:"), Content: content}
	if idx := strings.IndexByte(sessionKey, ':'); idx > 0 {
		msg.Channel, msg.ChatID = sessionKey[:idx], sessionKey[idx+1:]
	}
	t := &turn{steering: make(chan bus.InboundMessage, steeringBuffer)}
	return l.runTurn(ctx, msg, t)
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
	toolCtx    tools.ToolContext
}

func (l *Loop) runTurn(ctx context.Context, msg bus.InboundMessage, t *turn) (string, error) {
	reply, err := l.execute(ctx, turnInput{
		sessionKey: msg.SessionKey(),
		content:    msg.Content,
		toolCtx:    tools.ToolContext{Channel: msg.Channel, ChatID: msg.ChatID, SessionKey: msg.SessionKey()},
	}, t)
	if err == nil && l.ambient != nil {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.ambient.StoreExchange(msg.Content, reply)
		}()
	}
	return reply, err
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
		resp, err := l.chat.Chat(ctx, &provider.Request{
			Messages:    messages,
			Tools:       l.registry.Definitions(),
			MaxTokens:   l.cfg.Provider.MaxTokens,
			Temperature: l.cfg.Provider.Temperature,
		})
		if err != nil {
			if provider.IsContextOverflow(err) && !in.ephemeral && overflowRecoveries < 2 {
				overflowRecoveries++
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

		for _, call := range resp.ToolCalls {
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
		}
		messages = append(messages, l.drainSteering(in, t)...)
	}

	l.maybeCompactAsync(in)
	return "I hit the tool-iteration limit for this turn without reaching a final answer. Ask me to continue if you want me to keep going.", nil
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
			return out
		}
	}
}

func (l *Loop) recordLastChannel(msg bus.InboundMessage) {
	if msg.Channel == "cli" || msg.Channel == "system" {
		return
	}
	l.lastMu.Lock()
	l.lastChannel = msg
	l.lastMu.Unlock()
}

// LastChannel returns the most recent external channel/chat, for heartbeat
// and cron delivery. ok is false before any external message arrived.
func (l *Loop) LastChannel() (channel, chatID string, ok bool) {
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	if l.lastChannel.Channel == "" {
		return "", "", false
	}
	return l.lastChannel.Channel, l.lastChannel.ChatID, true
}
