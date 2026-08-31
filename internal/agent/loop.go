package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/cost"
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

// claimRetry is how often a synchronous turn that must own its session
// re-checks whether the live one has ended.
const claimRetry = 200 * time.Millisecond

// interruptedTool stands in for a tool result a cancelled turn never got. It
// says the turn was interrupted and says nothing about the tool, because the
// next turn reads this line as fact — and a bare "context canceled" reads as
// the tool being broken when all that happened is that the user spoke again.
const interruptedTool = "This tool call was cut short: the turn was interrupted before it finished. " +
	"That says nothing about whether the tool works — do not report it as a failure."

// turn tracks one in-flight session turn; extra inbound messages for the
// same session land in the steering queue and are injected between
// iterations instead of queuing a second turn.
type turn struct {
	steering chan bus.InboundMessage
	// audience is who could hear the turn when it was claimed. Steering is
	// refused across a widening of it: a message somebody else can hear must
	// not be answered by a turn whose recall was scoped to a private one.
	audience string
	// busDriven marks a turn dispatched off the inbound bus, whose reply
	// leaves on the outbound one — a chat, as opposed to a connector running
	// its own turn (voice, the phone) or a caller waiting synchronously
	// (cron, jobs). Only such a turn may ask a question on its own channel:
	// the answer has to be able to arrive the same way the question left.
	busDriven bool
	// ask, when non-nil, is a question ask_user has standing on this turn's
	// chat: the next inbound message the user writes there is its answer,
	// not steering. Guarded by Loop.mu, like the map this turn lives in.
	ask chan bus.InboundMessage
}

type Loop struct {
	cfg  *config.Config
	bus  *bus.MessageBus
	chat ChatProvider
	// utility is the chain for calls the user never reads. Nil means they
	// share the conversation's chain.
	utility  ChatProvider
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

	lastMu         sync.Mutex
	lastChannel    bus.InboundMessage
	reachable      func(channel string) bool
	conversational func(channel string) bool

	// seen is every session this process has run a turn for, with the tool
	// context it ran under. Idle compaction works from it rather than from
	// the session directory because the store names files by a sanitized key
	// it cannot invert — and compacting under a key that is nearly right
	// bills the cost to a session that does not exist.
	seenMu sync.Mutex
	seen   map[string]tools.ToolContext

	// pendingInduce is the last skill-worthy turn per session, waiting for
	// the session to go quiet before it is distilled into the catalog.
	induceMu      sync.Mutex
	pendingInduce map[string]induceCandidate

	windowFor func() int // context length of the models in play; 0 = unknown
}

func NewLoop(cfg *config.Config, b *bus.MessageBus, chat ChatProvider, registry *tools.Registry,
	sessions *session.Store, builder *ContextBuilder, ambient *memory.Ambient) *Loop {
	return &Loop{
		cfg:           cfg,
		bus:           b,
		chat:          chat,
		registry:      registry,
		sessions:      sessions,
		builder:       builder,
		ambient:       ambient,
		active:        map[string]*turn{},
		seen:          map[string]tools.ToolContext{},
		pendingInduce: map[string]induceCandidate{},
		sem:           make(chan struct{}, cfg.Agent.MaxConcurrentTurns),
		lastChannel:   loadLastChannel(),
	}
}

// WithUtility points the housekeeping calls — the compaction summary and the
// induction verdict — at their own chain. Nil leaves them on the main one.
func (l *Loop) WithUtility(chat ChatProvider) *Loop {
	l.utility = chat
	return l
}

// utilityChat is the chain for work the user never reads. It falls back to the
// conversation's own chain, so an unconfigured Factor behaves exactly as it
// did before the split existed.
func (l *Loop) utilityChat() ChatProvider {
	if l.utility != nil {
		return l.utility
	}
	return l.chat
}

// Run drains the inbound bus until ctx is cancelled. One live turn per
// session key; overflow becomes steering. It also sweeps for sessions that
// have gone quiet, which is when tidying them is free.
func (l *Loop) Run(ctx context.Context) {
	sweep := time.NewTicker(idleSweepEvery)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return
		case <-sweep.C:
			l.compactIdleSessions(ctx)
			l.induceIdleSessions(ctx)
		case msg := <-l.bus.Inbound():
			l.dispatch(ctx, msg)
		}
	}
}

const (
	// idleSweepEvery is how often quiet sessions are looked for. Compaction
	// is not urgent — nobody is waiting on it — so the sweep is lazy.
	idleSweepEvery = 5 * time.Minute
	// idleCompactAfter is how long a session must sit untouched before it is
	// compacted unasked. Long enough that a pause for coffee does not spend
	// an LLM call, short enough that a conversation left in the morning is
	// tidy by the afternoon.
	idleCompactAfter = 20 * time.Minute
)

// compactIdleSessions compacts every session that has gone quiet and grown
// past the budget, so the summarizing call is paid while nobody is waiting
// rather than in front of the user's first message after they come back.
//
// It is the same compaction the end of a turn triggers, on a different clock.
// A session with a live turn is left alone: not because concurrent appends
// are unsafe — the cut offset is absolute against an append-only log for
// exactly that reason — but because a session being spoken to is not idle,
// and the turn that ends will consider it anyway.
func (l *Loop) compactIdleSessions(ctx context.Context) {
	for key, toolCtx := range l.idleSessions() {
		l.wg.Add(1)
		go func(key string, toolCtx tools.ToolContext) {
			defer l.wg.Done()
			slog.Info("compacting an idle session", "session", key)
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if err := l.compact(tools.WithToolContext(cctx, toolCtx), key); err != nil {
				slog.Warn("idle compaction failed", "session", key, "error", err)
			}
		}(key, toolCtx)
	}
}

// idleSessions returns the sessions worth compacting right now, resolved
// under the locks before any of the work starts.
func (l *Loop) idleSessions() map[string]tools.ToolContext {
	l.seenMu.Lock()
	candidates := make(map[string]tools.ToolContext, len(l.seen))
	for key, toolCtx := range l.seen {
		candidates[key] = toolCtx
	}
	l.seenMu.Unlock()

	l.mu.Lock()
	for key := range l.active {
		delete(candidates, key)
	}
	l.mu.Unlock()

	for key := range candidates {
		last, ok := l.sessions.LastActivity(key)
		if !ok || time.Since(last) < idleCompactAfter {
			delete(candidates, key)
			continue
		}
		history, err := l.sessions.History(key)
		if err != nil || !l.needsCompaction(history) {
			delete(candidates, key)
		}
	}
	return candidates
}

// noteSession records a session as one this process can tidy later.
func (l *Loop) noteSession(in turnInput) {
	l.seenMu.Lock()
	defer l.seenMu.Unlock()
	l.seen[in.sessionKey] = in.toolCtx
}

// claim registers a live turn for key, or steers msg into the existing one.
// The steering send happens under l.mu — the same lock release drains under —
// so a message can never slip into a turn that has already been released.
//
// steer says whether msg may be folded into a turn already running rather
// than becoming one of its own. It reports whether the caller owns the turn
// and, when it does not, whether msg was handed to the turn that is running.
// That second answer is what a synchronous caller needs: a message the live
// turn took is already on its way to an answer, and one it could not take
// must still be waited out rather than dropped on the floor.
func (l *Loop) claim(key string, msg *bus.InboundMessage, steer, busDriven bool) (t *turn, owned, steered bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if live, running := l.active[key]; running {
		if !steer {
			return nil, false, false
		}
		if widensAudience(msg.Audience, live.audience) {
			// The room filled up while this turn was running. Folding the new
			// message in would answer it out of a context recalled for a
			// private conversation, in front of whoever just arrived.
			slog.Info("not steering across a wider audience; waiting for the turn",
				"session", key, "turn_audience", live.audience, "message_audience", msg.Audience)
			return nil, false, false
		}
		if live.ask != nil && !msg.System {
			// The turn has a question standing on this chat, and this is the
			// user's next word there: it is the answer, not steering. A
			// subsystem's message never is — a job finishing while the user
			// is being asked something must not answer for them.
			select {
			case live.ask <- *msg:
				slog.Info("inbound message answers the turn's question", "session", key)
				return nil, false, true
			default:
				// The answer slot is already full; the rest is steering.
			}
		}
		select {
		case live.steering <- *msg:
			slog.Info("steering message into live turn", "session", key)
			return nil, false, true
		default:
			slog.Warn("steering queue full", "session", key)
			return nil, false, false
		}
	}
	t = &turn{steering: make(chan bus.InboundMessage, steeringBuffer), audience: msg.Audience, busDriven: busDriven}
	l.active[key] = t
	return t, true, false
}

// widensAudience reports whether a message reaches more people than the turn
// it would be folded into was claimed for. Only that direction is refused:
// the reverse — the guest left, and the message is private again — costs the
// turn nothing but the shared-only recall it already had.
func widensAudience(message, live string) bool {
	return message == tools.AudienceShared && live != tools.AudienceShared
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
	t, ok, _ := l.claim(key, &msg, true, true)
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
	return l.ProcessDirectNotice(ctx, content, sessionKey, "", "", nil)
}

// ProcessDirectNotice runs a synchronous turn outside the bus and reports
// what the agent says on its way to the answer to notice, as it says it —
// the connector running the turn is the only place that line can arrive
// while it is still news. It honors the one-live-turn-per-session invariant:
// if the session is busy (e.g. an overlapping cron firing), it waits for
// the claim instead of interleaving histories.
//
// Waiting is right where the reply has to come back on the medium the
// request arrived on — a phone call is held open by the caller, and an
// answer published to the bus would be dialled as a second one. Where the
// connector is also served by the outbound pump, ProcessDirectSteering is
// the entry point that does not make the user wait in silence.
func (l *Loop) ProcessDirectNotice(ctx context.Context, content, sessionKey, speaker, audience string,
	notice func(string)) (string, error) {
	return l.processDirect(ctx, false, content, sessionKey, speaker, audience, notice)
}

// ProcessDirectSteering is ProcessDirectNotice for a connector whose replies
// also reach the user through its own Send. Where the session already has a
// turn running — a finished background job reporting back, a cron result —
// the message is folded into that turn as steering instead of queuing behind
// it, and this call returns nothing to say: the answer arrives with the
// running turn's reply, on the same channel.
//
// Queuing was the alternative, and it is worse than it looks. The user gets
// no acknowledgement, nothing is logged, and the next thing they say cancels
// the turn still waiting for its claim — so the question is not answered
// late, it is answered never. Steering also puts their words in front of the
// model while they are still what the conversation is about.
func (l *Loop) ProcessDirectSteering(ctx context.Context, content, sessionKey, speaker, audience string,
	notice func(string)) (string, error) {
	return l.processDirect(ctx, true, content, sessionKey, speaker, audience, notice)
}

func (l *Loop) processDirect(ctx context.Context, steer bool, content, sessionKey, speaker, audience string,
	notice func(string)) (string, error) {
	msg := bus.InboundMessage{Channel: "cli", ChatID: strings.TrimPrefix(sessionKey, "cli:"),
		Content: content, Speaker: speaker, Audience: audience}
	if idx := strings.IndexByte(sessionKey, ':'); idx > 0 {
		msg.Channel, msg.ChatID = sessionKey[:idx], sessionKey[idx+1:]
	}
	var t *turn
	for {
		t2, owned, steered := l.claim(sessionKey, &msg, steer, false)
		if owned {
			t = t2
			break
		}
		if steered {
			return "", nil // the live turn has it; its reply is the answer
		}
		// Not steered: either this caller must own its turn, or the live one
		// could not take the message. Both mean waiting, and losing it is
		// not an option.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(claimRetry):
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
	// speaker names who said content, where the channel can tell. Blank is
	// the ordinary case: the chat is one person.
	speaker   string
	ephemeral bool
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
		speaker:    msg.Speaker,
		notice:     notice,
		toolCtx: tools.ToolContext{Channel: msg.Channel, ChatID: msg.ChatID,
			SessionKey: msg.SessionKey(), Audience: msg.Audience},
	}, t)
	// A budget cap is a decision the user made, so it is answered rather
	// than reported as a breakage — and left out of memory, since "you ran
	// out of budget" is not a fact about the user worth recalling.
	if stop := cost.BudgetStop(err); stop != "" {
		return stop, nil
	}
	if err == nil && l.ambient != nil {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			// The speaker goes to memory as who said it, not as part of what
			// was said: the graph decides how to record a person, and a tag
			// meant for the prompt has no business becoming remembered text.
			l.ambient.StoreExchange(msg.Channel, msg.Audience, msg.Speaker, msg.Content, reply)
		}()
	}
	return reply, err
}

// markSpeaker tags a message with who said it, for the model's benefit. A
// bracketed name reads as an annotation rather than as something the user
// typed, which "Roxana: …" would not — that is a line a person could
// plausibly have spoken. Blank speaker, blank tag: the ordinary case where
// the conversation is one person and saying so every turn is noise.
func markSpeaker(speaker, content string) string {
	if speaker == "" {
		return content
	}
	return "[" + speaker + "] " + content
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
		l.noteSession(in)
	}

	systemPrompt := l.builder.SystemPrompt()
	// The gap is read before the turn's own message is written, since writing
	// it is what makes the session recent again.
	turnContext := l.builder.TurnContext(ctx, history, in.content, l.gapSince(in))
	// The speaker is marked on the message rather than in the prompt head:
	// the head is shared by every session and cached, and a name that
	// changes per utterance would fork it for the price of one line.
	userMsg := provider.Message{Role: "user", Content: markSpeaker(in.speaker, in.content)}

	// The per-turn block sits between the conversation so far and this turn,
	// and a mid-turn rebuild has to put it back in the same seam. Counting
	// what the turn has written is what finds it again: the log is append-only,
	// so this turn is always its last thisTurn messages.
	thisTurn := 0
	record := func(msg provider.Message) error {
		if err := l.persist(in, msg); err != nil {
			return err
		}
		thisTurn++
		return nil
	}
	if err := record(userMsg); err != nil {
		return "", err
	}

	messages := l.assemble(systemPrompt, turnContext, in.sessionKey, in.ephemeral,
		history, []provider.Message{userMsg})
	overflowRecoveries := 0

	for iteration := 0; iteration < l.cfg.Agent.MaxToolIterations; iteration++ {
		messages = l.trimInFlight(messages)
		markTail(messages)
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
				seam := max(len(fresh)-thisTurn, 0)
				messages = l.assemble(systemPrompt, turnContext, in.sessionKey, in.ephemeral,
					fresh[:seam], fresh[seam:])
				iteration--
				continue
			}
			return "", err
		}

		assistantMsg := provider.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls}
		if err := record(assistantMsg); err != nil {
			return "", err
		}
		messages = append(messages, assistantMsg)

		if len(resp.ToolCalls) == 0 {
			// Final answer — unless steering arrived mid-turn; then keep going.
			steered := l.drainSteering(in, t, record)
			if len(steered) == 0 {
				l.noteInduceCandidate(in, messages, thisTurn, resp.Content)
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
			// Whoever is running this turn hears the line from the turn
			// itself, which is the one place it arrives in time to be worth
			// saying. Only a turn nobody is holding sends it the long way,
			// off the bus — otherwise the connector says it twice.
			if in.notice != nil {
				in.notice(notice)
			} else {
				l.emit(in.sessionKey, PhaseNotice, notice)
			}
		}

		// The batch is answered before any of it is written down, so the
		// calls can run in whatever order suits them and still be recorded
		// in the order the model asked for.
		outcomes := l.runTools(ctx, in.sessionKey, resp.ToolCalls)
		for i, call := range resp.ToolCalls {
			toolMsg := provider.Message{Role: "tool", ToolCallID: call.ID, Content: outcomes[i].content}
			if err := record(toolMsg); err != nil {
				return "", err
			}
			messages = append(messages, toolMsg)

			// A tool result carrying images (screen_view's annotated
			// screenshot) rides a follow-up user message — the one placement
			// every vision dialect accepts. Only the placeholder is persisted:
			// session files stay small, replay stays valid on any model, and
			// a stale frame is worthless next turn anyway.
			if len(outcomes[i].images) > 0 {
				if err := record(provider.Message{Role: "user", Content: imagePruned(call.Name)}); err != nil {
					return "", err
				}
				messages = append(messages, provider.Message{
					Role:    "user",
					Content: fmt.Sprintf("[Attached: image output of the %s tool. This is tool output, not a message from the user.]", call.Name),
					Images:  outcomes[i].images,
				})
				pruneImages(messages)
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		messages = append(messages, l.drainSteering(in, t, record)...)
	}

	reply := l.wrapUp(ctx, in, messages)
	l.maybeCompactAsync(in)
	return reply, nil
}

// maxParallelTools bounds how much of one batch runs at once. A batch is
// normally two or three calls, so the ceiling exists for the pathological one
// — thirty fetches asked for in a single breath — where the limit protects the
// hosts on the other end rather than this process.
const maxParallelTools = 6

// toolOutcome is one call's answer, resolved before anything is written down.
type toolOutcome struct {
	content string
	images  []provider.ImagePart
}

// runTools answers one batch of tool calls, in the order they were asked.
//
// A model that asks for the same tool with the same arguments twice in one
// batch is repeating itself, not asking for a second reading. Both ids still
// need a result beside them or the history stops replaying, so the first
// answer serves both — which spares the second run of whatever the call does,
// and the iteration it would have cost.
//
// The distinct calls run at the same time when every one of them has declared
// itself safe to (tools.Parallel). That is the difference between a batch
// costing the sum of its round trips and costing the longest of them, and
// nearly every tool here spends its time waiting on something else. Most
// batches are not eligible and run exactly as they always have: order is
// load-bearing for a pointer, a browser and a file write, and a tool that has
// not declared itself is assumed to be one of those.
func (l *Loop) runTools(ctx context.Context, sessionKey string, calls []provider.ToolCall) []toolOutcome {
	outcomes := make([]toolOutcome, len(calls))

	firstOf := map[string]int{}
	sameAs := make([]int, len(calls))
	var distinct []int
	for i, call := range calls {
		sig := callSignature(call)
		if j, repeat := firstOf[sig]; repeat {
			sameAs[i] = j
			continue
		}
		firstOf[sig] = i
		sameAs[i] = i
		distinct = append(distinct, i)
	}

	run := func(i int) {
		call := calls[i]
		if ctx.Err() != nil {
			// The turn is over — on voice, because the user talked over it.
			// The result must not read as the tool having failed: the next
			// turn treats what it finds here as evidence, and a "context
			// canceled" left by an interruption has been read back as a
			// broken browser.
			outcomes[i] = toolOutcome{content: interruptedTool}
			return
		}
		l.emit(sessionKey, PhaseTool, call.Name)
		result := l.registry.Execute(ctx, call.Name, call.Args)
		content := result.ForLLM
		if result.IsError {
			content = "ERROR: " + content
		}
		if ctx.Err() != nil {
			// Cancelled while the tool was running: whatever it returned is
			// the cancellation, not a verdict on the tool.
			outcomes[i] = toolOutcome{content: interruptedTool}
			return
		}
		outcomes[i] = toolOutcome{content: content, images: result.Images}
	}

	if l.batchRunsInParallel(calls, distinct) {
		var wg sync.WaitGroup
		gate := make(chan struct{}, maxParallelTools)
		for _, i := range distinct {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				gate <- struct{}{}
				defer func() { <-gate }()
				run(i)
			}(i)
		}
		wg.Wait()
	} else {
		for _, i := range distinct {
			run(i)
		}
	}

	for i := range calls {
		if j := sameAs[i]; j != i {
			// The repeat gets the answer but not a second copy of the
			// picture: one frame of a screen is the frame.
			outcomes[i] = toolOutcome{content: outcomes[j].content}
		}
	}
	return outcomes
}

// batchRunsInParallel reports whether a whole batch may run at once. One
// undeclared call makes the whole batch sequential, because the guarantee is
// about the batch and not about a tool: a file read running beside a mouse
// click is still a read racing a click.
func (l *Loop) batchRunsInParallel(calls []provider.ToolCall, distinct []int) bool {
	if len(distinct) < 2 {
		return false
	}
	for _, i := range distinct {
		if !l.registry.ParallelSafe(calls[i].Name, calls[i].Args) {
			return false
		}
	}
	return true
}

// callSignature identifies a call by what it would do: the tool and the
// arguments, rendered canonically (json.Marshal sorts map keys).
func callSignature(call provider.ToolCall) string {
	args, err := json.Marshal(call.Args)
	if err != nil {
		return ""
	}
	return call.Name + string(args)
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
		Messages:    l.trimInFlight(append(messages, nudge)),
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

// assemble builds the request message list: the stable prefix first — system
// prompt, rolling summary, the conversation so far with its older tool
// results masked — then the per-turn block, then what this turn has said.
//
// The order is the point. Everything a prompt cache can reuse comes first and
// byte-identical, so the divergence between one turn's request and the next
// falls at the per-turn block instead of a few hundred tokens into the system
// prompt, where it used to leave the whole history uncacheable. The turn's own
// messages sit past it so the last thing the model reads is still what the
// user actually said.
func (l *Loop) assemble(systemPrompt, turnContext, sessionKey string, ephemeral bool,
	prior, current []provider.Message) []provider.Message {
	// Masked over both halves at once: how recent a tool result is depends on
	// the whole request, not on which side of the block it fell.
	masked := maskOldToolResults(append(append(make([]provider.Message, 0, len(prior)+len(current)), prior...), current...))

	messages := make([]provider.Message, 0, len(masked)+3)
	messages = append(messages, provider.Message{Role: "system", Content: systemPrompt})
	if !ephemeral {
		if summary := l.sessions.Summary(sessionKey); summary != "" {
			messages = append(messages, provider.Message{Role: "system", Content: "Summary of earlier conversation:\n" + summary})
		}
	}
	// Each system message ends a stretch worth caching on its own. The prompt
	// is invariant by construction; the summary under it is not, and is
	// rewritten every time compaction runs. Marking both means a new summary
	// costs its own entry and not the tool schemas and system prompt in front
	// of it, which are the largest fixed block in the request.
	for i := range messages {
		if messages[i].Role == "system" {
			messages[i].CacheMark = true
		}
	}

	messages = append(messages, masked[:len(prior)]...)
	// Everything to here is what the session's next turn repeats verbatim.
	// turnContext is where two consecutive turns first differ, so the mark
	// belongs in front of it rather than after.
	if n := len(messages) - 1; n >= 0 {
		messages[n].CacheMark = true
	}

	if turnContext != "" {
		messages = append(messages, provider.Message{Role: "user", Content: turnContext})
	}
	return append(messages, masked[len(prior):]...)
}

// markTail puts a cache breakpoint at the end of the request as it stands.
//
// The marks assemble places are fixed for the whole turn, which leaves the
// part that actually grows — up to twenty iterations of tool calls and the
// results they return — reprocessed from scratch on every one of those
// iterations. A mark at the tail before each call gives the next iteration
// something to read instead.
//
// Marks accumulate rather than move, and the dialect thins them to the few it
// may send: the fixed head every iteration re-reads, and the most recent
// tails, which is where this request diverges from the last one. An older tail
// mark is stale by then anyway. Marking has to keep pace with the appends
// because a breakpoint searches back only a bounded number of content blocks
// for an earlier entry — an iteration adds two or three, so one mark per
// iteration stays well inside that window, and no mark at all falls out of it
// within a few tool calls.
func markTail(messages []provider.Message) {
	if n := len(messages) - 1; n >= 0 {
		messages[n].CacheMark = true
	}
}

// gapSince reports how long the session has been untouched, for the notice
// that tells a returning user's turn it is not a continuation. Zero for a
// session with no history to be away from, and for an ephemeral turn, which
// has no session to return to.
func (l *Loop) gapSince(in turnInput) time.Duration {
	if in.ephemeral {
		return 0
	}
	last, ok := l.sessions.LastActivity(in.sessionKey)
	if !ok {
		return 0
	}
	return time.Since(last)
}

func (l *Loop) persist(in turnInput, msg provider.Message) error {
	if in.ephemeral {
		return nil
	}
	return l.sessions.Append(in.sessionKey, msg)
}

func (l *Loop) drainSteering(in turnInput, t *turn, record func(provider.Message) error) []provider.Message {
	var out []provider.Message
	for {
		select {
		case msg := <-t.steering:
			// Who said it travels with a steered message too: on voice the
			// turn's own speaker is whoever opened it, and the person who
			// spoke over it may not be the same one.
			userMsg := provider.Message{Role: "user", Content: markSpeaker(msg.Speaker, msg.Content)}
			if err := record(userMsg); err != nil {
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

// SetContextWindow teaches the loop how much context the models in play
// carry, so compaction triggers against the real window instead of an
// assumption. fn answers 0 when nothing knows — the loop then falls back to
// config, and past that to a fixed default. It is consulted on every check:
// the answer can improve after startup, once the model catalog arrives.
func (l *Loop) SetContextWindow(fn func() int) {
	l.windowFor = fn
}

// SetReachable teaches the loop which channels have a connector behind them
// right now, so LastChannel never hands out an address nothing can deliver
// to. Unset — the CLI, tests — every channel counts as reachable.
func (l *Loop) SetReachable(fn func(channel string) bool) {
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	l.reachable = fn
}

// SetConversational teaches the loop which channels are running chats whose
// conversation rides the bus both ways — where a question published outbound
// lands as a message and the user's reply comes back inbound. A connector
// that runs its own turns (voice, the phone) is not one, whatever else it
// publishes: text pushed onto the bus for it is spoken or dialled, not
// threaded into a chat. Unset — the CLI, tests — every channel counts.
func (l *Loop) SetConversational(fn func(channel string) bool) {
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	l.conversational = fn
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
