package phone

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
)

// The bridge is the agent's half of the voice link: an OpenAI-compatible
// chat-completions endpoint on loopback that the voice shell drives as its
// "custom LLM". Every request is one conversational turn, run synchronously
// through the agent loop under the caller's own session key — so history and
// memory work with no extra machinery, and hanging up (or talking over the
// agent) cancels the HTTP request, which cancels the turn.
//
// It never leaves 127.0.0.1 and it is guarded by a bearer secret regenerated
// on every boot and handed to the voice shell through its environment.

// TurnFunc runs one synchronous turn (wired to Loop.ProcessDirect).
type TurnFunc func(ctx context.Context, content, sessionKey string) (string, error)

// origin is the session a call was started from, so its outcome can be
// reported back where the user asked for it.
type origin struct {
	Channel string
	ChatID  string
}

// callInfo is what the bridge remembers about a live call.
type callInfo struct {
	From      string
	To        string
	Direction string
	Goal      string
	Origin    origin
	briefed   bool // the goal has been handed to the model already
}

const (
	directionInbound  = "inbound"
	directionOutbound = "outbound"

	// transcriptTailLimit bounds what a finished call injects back into the
	// originating session.
	transcriptTailLimit = 1500

	// spokenFailure is what the caller hears when a turn fails outright.
	// Dead air is a worse failure mode than an apology, and the error text
	// itself never reaches the phone line.
	spokenFailure = "Sorry — something went wrong on my end. Could you say that again?"
)

type bridge struct {
	token   string
	allow   func(number string) bool
	publish func(bus.InboundMessage) bool

	mu      sync.Mutex
	run     TurnFunc
	calls   map[string]*callInfo
	aliases map[string]string // voice shell session id -> call id

	ln  net.Listener
	srv *http.Server
	wg  sync.WaitGroup
}

func newBridge(token string, allow func(string) bool, publish func(bus.InboundMessage) bool) *bridge {
	if publish == nil {
		publish = func(bus.InboundMessage) bool { return false }
	}
	if allow == nil {
		allow = func(string) bool { return true }
	}
	return &bridge{
		token:   token,
		allow:   allow,
		publish: publish,
		calls:   map[string]*callInfo{},
		aliases: map[string]string{},
	}
}

func (b *bridge) bindRunner(run TurnFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.run = run
}

func (b *bridge) runner() TurnFunc {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.run
}

func (b *bridge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", b.handleChat)
	mux.HandleFunc("POST /internal/call-event", b.handleCallEvent)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

// listen binds eagerly so a port clash is reported by Start, not swallowed by
// a background goroutine.
func (b *bridge) listen(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		return fmt.Errorf("voice bridge listener on 127.0.0.1:%d: %w", port, err)
	}
	b.ln = ln
	b.srv = &http.Server{Handler: b.handler(), ReadHeaderTimeout: 5 * time.Second}
	return nil
}

func (b *bridge) serve() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if err := b.srv.Serve(b.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("voice bridge failed", "error", err)
		}
	}()
}

func (b *bridge) shutdown() {
	if b.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = b.srv.Shutdown(ctx)
	b.wg.Wait()
}

// port reports the address the bridge actually bound (tests ask for :0).
func (b *bridge) port() int {
	if b.ln == nil {
		return 0
	}
	addr, ok := b.ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

func (b *bridge) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(b.token)) == 1
}

// ---- chat completions ------------------------------------------------------

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	User     string        `json:"user"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

const bridgeModel = "factor"

func (b *bridge) handleChat(w http.ResponseWriter, r *http.Request) {
	if !b.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	run := b.runner()
	if run == nil {
		http.Error(w, "the agent loop is not attached to this bridge", http.StatusServiceUnavailable)
		return
	}

	callID := b.resolveCallID(r.Header.Get("X-Factor-Call-Id"))
	info := b.lookupCall(callID)
	caller := b.resolveCaller(info, r, req)
	if caller == "" {
		http.Error(w, "no caller number on the request (X-Factor-Caller)", http.StatusBadRequest)
		return
	}
	// Second gate. The voice shell hangs up on callers who are not on the
	// allowlist; if that ever fails, the agent still never speaks to them.
	if !b.allow(caller) {
		slog.Warn("refusing a voice turn from a caller outside the allowlist", "caller", caller, "call", callID)
		http.Error(w, "caller is not allowed", http.StatusForbidden)
		return
	}

	content := lastUserMessage(req.Messages)
	if strings.TrimSpace(content) == "" {
		http.Error(w, "no user message in the request", http.StatusBadRequest)
		return
	}
	content = b.brief(info, r, content)

	reply, err := run(r.Context(), content, "phone:"+caller)
	if r.Context().Err() != nil {
		// Barge-in or hang-up: the shell already abandoned this request.
		return
	}
	if err != nil {
		slog.Error("voice turn failed", "caller", caller, "call", callID, "error", err)
		reply = spokenFailure
	}
	if strings.TrimSpace(reply) == "" {
		reply = "…"
	}

	if req.Stream {
		b.writeSSE(w, reply)
		return
	}
	writeJSON(w, http.StatusOK, chatResponse{
		ID:      completionID(callID),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   bridgeModel,
		Choices: []chatChoice{{
			Index:        0,
			Message:      &chatMessage{Role: "assistant", Content: reply},
			FinishReason: finish("stop"),
		}},
		Usage: &chatUsage{},
	})
}

// sessionIDPrefix is what the voice shell's LLM adapter stamps in front of a
// call id when it tags a request.
const sessionIDPrefix = "patter-call-"

// resolveCallID maps the tag on a chat request back to the call it belongs to.
func (b *bridge) resolveCallID(header string) string {
	id := strings.TrimPrefix(strings.TrimSpace(header), sessionIDPrefix)
	if id == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if local, ok := b.aliases[id]; ok {
		return local
	}
	return id
}

// lookupCall finds the call a request belongs to. A single live call is
// unambiguous — and Factor is a single-user agent — so a tag that resolves to
// nothing while one call is up is that call, rather than a dropped turn.
func (b *bridge) lookupCall(callID string) *callInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	if info, known := b.calls[callID]; known {
		return info
	}
	if len(b.calls) == 1 {
		for _, only := range b.calls {
			return only
		}
	}
	return nil
}

// resolveCaller identifies the person on the line. The call registry is the
// source of truth — the voice shell registers caller and callee the moment a
// call connects — with the header and the OpenAI `user` field as fallbacks, so
// a shell that learns to stamp the number directly needs no change here.
func (b *bridge) resolveCaller(info *callInfo, r *http.Request, req chatRequest) string {
	if info != nil {
		b.mu.Lock()
		to, from, direction := info.To, info.From, info.Direction
		b.mu.Unlock()
		if direction == directionOutbound && validNumber(to) {
			return to
		}
		if validNumber(from) {
			return from
		}
	}
	if n := normalizeNumber(r.Header.Get("X-Factor-Caller")); validNumber(n) {
		return n
	}
	if n := normalizeNumber(req.User); validNumber(n) {
		return n
	}
	return ""
}

// brief prefixes the first turn of a call Factor placed with why it called, so
// the model opens the conversation on purpose instead of guessing. Direction
// and goal come from the call registry rather than request headers: the voice
// shell's LLM adapter can tag a request with its call, but not with arbitrary
// metadata. A header still wins where one is present.
func (b *bridge) brief(info *callInfo, r *http.Request, content string) string {
	if info == nil {
		return content
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	outbound := info.Direction == directionOutbound || r.Header.Get("X-Factor-Direction") == directionOutbound
	if !outbound || info.briefed {
		return content
	}
	info.briefed = true

	goal := strings.TrimSpace(r.Header.Get("X-Factor-Goal"))
	if goal == "" {
		goal = info.Goal
	}
	if goal == "" {
		return content
	}
	return fmt.Sprintf("[voice call you initiated: %s]\n%s", goal, content)
}

func lastUserMessage(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// writeSSE emits the reply as a spec-shaped chat-completion stream. Factor's
// provider layer is not streaming, so the whole turn arrives as one delta —
// the framing is what the voice shell needs, not the token cadence.
func (b *bridge) writeSSE(w http.ResponseWriter, reply string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id := completionID("")
	created := time.Now().Unix()
	chunk := func(delta chatMessage, reason *string) {
		payload, err := json.Marshal(chatResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   bridgeModel,
			Choices: []chatChoice{{Index: 0, Delta: &delta, FinishReason: reason}},
		})
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk(chatMessage{Role: "assistant"}, nil)
	chunk(chatMessage{Content: reply}, nil)
	chunk(chatMessage{}, finish("stop"))
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func finish(reason string) *string { return &reason }

func completionID(callID string) string {
	if callID == "" {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + callID
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// ---- call lifecycle --------------------------------------------------------

type callEvent struct {
	Event  string `json:"event"` // call_started | call_ended
	CallID string `json:"call_id"`
	// SessionID is the voice shell's own id for the call, which is what its
	// LLM adapter tags chat requests with. An outbound call has both: the id
	// Factor was handed when it asked for the call, and the one the shell
	// learned when the carrier connected it.
	SessionID  string `json:"session_id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Direction  string `json:"direction"`
	Status     string `json:"status"`
	Transcript string `json:"transcript"`
}

// registerOutbound records where an outbound call was asked for, before the
// shell reports anything, so its outcome lands in the right conversation.
func (b *bridge) registerOutbound(callID, to, goal string, from origin) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls[callID] = &callInfo{
		To:        normalizeNumber(to),
		Direction: directionOutbound,
		Goal:      goal,
		Origin:    from,
	}
}

func (b *bridge) handleCallEvent(w http.ResponseWriter, r *http.Request) {
	if !b.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ev callEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&ev); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch ev.Event {
	case "call_started":
		b.callStarted(ev)
	case "call_ended":
		b.callEnded(ev)
	default:
		http.Error(w, "unknown event "+ev.Event, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (b *bridge) callStarted(ev callEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	info, known := b.calls[ev.CallID]
	if !known {
		info = &callInfo{Origin: origin{}}
		b.calls[ev.CallID] = info
	}
	if ev.SessionID != "" && ev.SessionID != ev.CallID {
		b.aliases[ev.SessionID] = ev.CallID
	}
	info.From = normalizeNumber(ev.From)
	if to := normalizeNumber(ev.To); to != "" {
		info.To = to
	}
	if ev.Direction != "" {
		info.Direction = ev.Direction
	}
	if info.Direction == "" {
		info.Direction = directionInbound
	}
	slog.Info("call started", "call", ev.CallID, "direction", info.Direction, "from", info.From, "to", info.To)
}

func (b *bridge) callEnded(ev callEvent) {
	b.mu.Lock()
	info := b.calls[ev.CallID]
	delete(b.calls, ev.CallID)
	delete(b.aliases, ev.SessionID)
	b.mu.Unlock()

	slog.Info("call ended", "call", ev.CallID, "status", ev.Status)
	if info == nil || info.Origin.Channel == "" {
		return // inbound calls report themselves: the conversation already happened
	}
	b.publish(bus.InboundMessage{
		Channel: info.Origin.Channel,
		ChatID:  info.Origin.ChatID,
		Content: outcomeReport(info.To, ev.Status, ev.Transcript),
		Time:    time.Now(),
	})
}

// outcomeReport turns a finished outbound call into the message that re-enters
// the originating session — the background-jobs pattern, so the agent reports
// back wherever the user asked for the call.
func outcomeReport(to, status, transcript string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[system] Outbound call to %s ", to)
	switch status {
	case "completed":
		b.WriteString("finished: the conversation completed.")
	case "no-answer":
		b.WriteString("was not answered.")
	case "busy":
		b.WriteString("got a busy signal — the line was engaged.")
	case "voicemail":
		b.WriteString("reached voicemail instead of a person.")
	case "failed":
		b.WriteString("failed to connect (the carrier rejected it).")
	case "rejected":
		b.WriteString("was rejected: the number is not on the outbound allowlist.")
	default:
		fmt.Fprintf(&b, "ended with status %q.", status)
	}
	if tail := strings.TrimSpace(transcript); tail != "" {
		if len(tail) > transcriptTailLimit {
			tail = "…" + tail[len(tail)-transcriptTailLimit:]
		}
		b.WriteString("\nTranscript tail:\n")
		b.WriteString(tail)
	}
	b.WriteString("\n\nReport the outcome to the user concisely.")
	return b.String()
}
