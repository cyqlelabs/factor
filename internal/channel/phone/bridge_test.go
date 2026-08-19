package phone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cyqlelabs/factor/internal/bus"
)

// unauthorizedServer answers every request with a 401: a server that is
// clearly there but refusing, which several probes have to treat as "present".
func unauthorizedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type testBridge struct {
	*bridge
	url      string
	inbound  chan bus.InboundMessage
	turns    chan string
	sessions chan string
}

// newTestBridge starts a real bridge on a real loopback port; the tests drive
// it with a real HTTP client, because the wire format is the contract.
func newTestBridge(t *testing.T, run TurnFunc) *testBridge {
	t.Helper()
	tb := &testBridge{
		inbound:  make(chan bus.InboundMessage, 8),
		turns:    make(chan string, 8),
		sessions: make(chan string, 8),
	}
	tb.bridge = newBridge("bridge-secret", func(string) bool { return true },
		func(msg bus.InboundMessage) bool {
			tb.inbound <- msg
			return true
		})
	if run == nil {
		run = func(_ context.Context, content, sessionKey string, _ func(string)) (string, error) {
			tb.turns <- content
			tb.sessions <- sessionKey
			return "the agent replied", nil
		}
	}
	tb.bindRunner(run)
	if err := tb.listen(0); err != nil {
		t.Fatal(err)
	}
	tb.serve()
	t.Cleanup(tb.shutdown)
	tb.url = fmt.Sprintf("http://127.0.0.1:%d", tb.port())
	return tb
}

func (tb *testBridge) post(t *testing.T, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, tb.url+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bridge-secret")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// chatBody is a minimal OpenAI chat-completions request, the shape Patter's
// custom-LLM adapter sends.
func chatBody(text string, stream bool) map[string]any {
	return map[string]any{
		"model":  "factor",
		"stream": stream,
		"messages": []map[string]string{
			{"role": "system", "content": "ignored: Factor builds its own prompt"},
			{"role": "user", "content": text},
		},
	}
}

// startCall registers a live call the way the voice shell does.
func (tb *testBridge) startCall(t *testing.T, callID, sessionID, from string) {
	t.Helper()
	resp := tb.post(t, "/internal/call-event", callEvent{
		Event: "call_started", CallID: callID, SessionID: sessionID,
		From: from, To: "+15550002222", Direction: directionInbound,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("call_started = HTTP %d", resp.StatusCode)
	}
}

func TestBridgeStreamsASpecShapedCompletion(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.startCall(t, "CA1", "CA1", "+15550001111")

	resp := tb.post(t, "/v1/chat/completions", chatBody("what is on my calendar", true),
		map[string]string{"X-Factor-Call-Id": sessionIDPrefix + "CA1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q, want text/event-stream", ct)
	}

	var events []string
	var assembled strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		payload, found := strings.CutPrefix(line, "data: ")
		if !found {
			t.Fatalf("frame %q is not an SSE data line", line)
		}
		events = append(events, payload)
		if payload == "[DONE]" {
			break
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk %q is not JSON: %v", payload, err)
		}
		if chunk.Object != "chat.completion.chunk" || len(chunk.Choices) != 1 {
			t.Errorf("malformed chunk: %+v", chunk)
		}
		if chunk.Choices[0].Delta == nil {
			t.Fatalf("chunk %q has no delta", payload)
		}
		assembled.WriteString(chunk.Choices[0].Delta.Content)
	}
	if assembled.String() != "the agent replied" {
		t.Errorf("assembled stream = %q", assembled.String())
	}
	if len(events) == 0 || events[len(events)-1] != "[DONE]" {
		t.Errorf("stream did not terminate with [DONE]: %v", events)
	}
	if got := <-tb.sessions; got != "phone:+15550001111" {
		t.Errorf("session key = %q, want the caller's own", got)
	}
}

func TestBridgeAnswersNonStreamingRequests(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.startCall(t, "CA2", "CA2", "+15550001111")

	resp := tb.post(t, "/v1/chat/completions", chatBody("hello", false),
		map[string]string{"X-Factor-Call-Id": "CA2"})
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Object != "chat.completion" || len(parsed.Choices) != 1 {
		t.Fatalf("response = %+v", parsed)
	}
	if parsed.Choices[0].Message == nil || parsed.Choices[0].Message.Content != "the agent replied" {
		t.Errorf("message = %+v", parsed.Choices[0].Message)
	}
	if parsed.Choices[0].FinishReason == nil || *parsed.Choices[0].FinishReason != "stop" {
		t.Errorf("finish reason = %v", parsed.Choices[0].FinishReason)
	}
}

func TestBridgeRejectsTheWrongBearer(t *testing.T) {
	tb := newTestBridge(t, nil)
	for _, header := range []string{"", "Bearer wrong", "bridge-secret"} {
		req, err := http.NewRequest(http.MethodPost, tb.url+"/v1/chat/completions",
			strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q = HTTP %d, want 401", header, resp.StatusCode)
		}
	}
}

// Barge-in: Patter aborts the in-flight request when the caller talks over the
// agent, and that has to reach the turn or the agent keeps thinking into a
// conversation that has moved on.
func TestBridgeCancellationReachesTheTurn(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	tb := newTestBridge(t, func(ctx context.Context, _, _ string, _ func(string)) (string, error) {
		close(started)
		<-ctx.Done()
		cancelled <- ctx.Err()
		return "", ctx.Err()
	})
	tb.startCall(t, "CA3", "CA3", "+15550001111")

	ctx, cancel := context.WithCancel(context.Background())
	raw, err := json.Marshal(chatBody("keep going", true))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tb.url+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bridge-secret")
	req.Header.Set("X-Factor-Call-Id", "CA3")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never started")
	}
	cancel()
	select {
	case err := <-cancelled:
		if err == nil {
			t.Error("the turn was released without a cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hanging up did not cancel the turn")
	}
	<-done
}

func TestBridgeRefusesWorkItCannotDo(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.startCall(t, "CA4", "CA4", "+15550001111")

	cases := []struct {
		name     string
		body     any
		headers  map[string]string
		wantCode int
	}{
		{"no user message", map[string]any{"messages": []map[string]string{{"role": "system", "content": "x"}}},
			map[string]string{"X-Factor-Call-Id": "CA4"}, http.StatusBadRequest},
		{"blank user message", chatBody("   ", false),
			map[string]string{"X-Factor-Call-Id": "CA4"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tb.post(t, "/v1/chat/completions", c.body, c.headers).StatusCode; got != c.wantCode {
				t.Errorf("status = %d, want %d", got, c.wantCode)
			}
		})
	}

	t.Run("malformed json", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, tb.url+"/v1/chat/completions", strings.NewReader("{not json"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer bridge-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

func TestBridgeWithoutACallerRefusesTheTurn(t *testing.T) {
	tb := newTestBridge(t, nil)
	resp := tb.post(t, "/v1/chat/completions", chatBody("who am i", false),
		map[string]string{"X-Factor-Call-Id": "unknown-call"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when the caller cannot be identified", resp.StatusCode)
	}
}

func TestBridgeFallsBackToTheHeaderAndUserField(t *testing.T) {
	tb := newTestBridge(t, nil)

	tb.post(t, "/v1/chat/completions", chatBody("hi", false),
		map[string]string{"X-Factor-Call-Id": "no-such-call", "X-Factor-Caller": "+1 (555) 000-7777"})
	if got := <-tb.sessions; got != "phone:+15550007777" {
		t.Errorf("session key from the header = %q", got)
	}

	body := chatBody("hi again", false)
	body["user"] = "+15550008888"
	tb.post(t, "/v1/chat/completions", body, map[string]string{"X-Factor-Call-Id": "also-unknown"})
	if got := <-tb.sessions; got != "phone:+15550008888" {
		t.Errorf("session key from the user field = %q", got)
	}
}

// A tag that resolves to nothing while exactly one call is live is not
// ambiguous — and Factor is a single-user agent, so refusing there would drop
// a conversation for a naming mismatch.
func TestBridgeResolvesTheOnlyLiveCall(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.startCall(t, "CA5", "session-5", "+15550001111")

	tb.post(t, "/v1/chat/completions", chatBody("hello", false),
		map[string]string{"X-Factor-Call-Id": "an-id-nobody-registered"})
	if got := <-tb.sessions; got != "phone:+15550001111" {
		t.Errorf("session key = %q, want the only live call's caller", got)
	}

	// The alias the shell reported resolves too.
	tb.post(t, "/v1/chat/completions", chatBody("still here", false),
		map[string]string{"X-Factor-Call-Id": sessionIDPrefix + "session-5"})
	if got := <-tb.sessions; got != "phone:+15550001111" {
		t.Errorf("session key via alias = %q", got)
	}
}

func TestBridgeRefusesCallersOutsideTheAllowlist(t *testing.T) {
	tb := &testBridge{inbound: make(chan bus.InboundMessage, 4), sessions: make(chan string, 4), turns: make(chan string, 4)}
	tb.bridge = newBridge("bridge-secret",
		func(number string) bool { return number == "+15550001111" },
		func(bus.InboundMessage) bool { return true })
	tb.bindRunner(func(_ context.Context, _, sessionKey string, _ func(string)) (string, error) {
		tb.sessions <- sessionKey
		return "should never be spoken", nil
	})
	if err := tb.listen(0); err != nil {
		t.Fatal(err)
	}
	tb.serve()
	t.Cleanup(tb.shutdown)
	tb.url = fmt.Sprintf("http://127.0.0.1:%d", tb.port())

	resp := tb.post(t, "/v1/chat/completions", chatBody("let me in", false),
		map[string]string{"X-Factor-Caller": "+15559999999"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a caller outside the allowlist", resp.StatusCode)
	}
	select {
	case key := <-tb.sessions:
		t.Errorf("the agent ran a turn for a rejected caller (%s)", key)
	default:
	}
}

func TestBridgeWithoutARunnerIsHonestAboutIt(t *testing.T) {
	b := newBridge("bridge-secret", nil, nil)
	if err := b.listen(0); err != nil {
		t.Fatal(err)
	}
	b.serve()
	t.Cleanup(b.shutdown)

	tb := &testBridge{bridge: b, url: fmt.Sprintf("http://127.0.0.1:%d", b.port())}
	resp := tb.post(t, "/v1/chat/completions", chatBody("anyone home", false),
		map[string]string{"X-Factor-Caller": "+15550001111"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with no agent attached", resp.StatusCode)
	}
}

// A failed turn must still produce speech: dead air on a phone call reads as a
// dropped call, and the error text itself must not be spoken aloud.
func TestBridgeSpeaksAnApologyWhenATurnFails(t *testing.T) {
	tb := newTestBridge(t, func(context.Context, string, string, func(string)) (string, error) {
		return "", fmt.Errorf("provider chain exhausted: api key sk-secret rejected")
	})
	resp := tb.post(t, "/v1/chat/completions", chatBody("hello", false),
		map[string]string{"X-Factor-Caller": "+15550001111"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want a spoken reply rather than an HTTP error", resp.StatusCode)
	}
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	spoken := parsed.Choices[0].Message.Content
	if spoken != spokenFailure {
		t.Errorf("spoken reply = %q, want the apology", spoken)
	}
	if strings.Contains(spoken, "sk-secret") {
		t.Error("the failure detail was read out to the caller")
	}
}

func TestBridgeNeverSpeaksAnEmptyReply(t *testing.T) {
	tb := newTestBridge(t, func(context.Context, string, string, func(string)) (string, error) { return "  ", nil })
	resp := tb.post(t, "/v1/chat/completions", chatBody("hello", false),
		map[string]string{"X-Factor-Caller": "+15550001111"})
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		t.Error("an empty completion would leave the line silent")
	}
}

// The goal of a call Factor placed belongs on the first turn only; repeating
// it every turn would have the agent re-introduce itself indefinitely.
func TestBridgeBriefsAnOutboundCallOnce(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.registerOutbound("out-1", "+15550001111", "ask about the dentist appointment", origin{Channel: "telegram", ChatID: "42"})
	tb.post(t, "/internal/call-event", callEvent{
		Event: "call_started", CallID: "out-1", SessionID: "CA9",
		From: "+15550002222", To: "+15550001111", Direction: directionOutbound,
	}, nil)

	headers := map[string]string{
		"X-Factor-Call-Id":   sessionIDPrefix + "CA9",
		"X-Factor-Direction": directionOutbound,
		"X-Factor-Goal":      "ask about the dentist appointment",
	}
	tb.post(t, "/v1/chat/completions", chatBody("hello?", false), headers)
	first := <-tb.turns
	if !strings.HasPrefix(first, "[voice call you initiated: ask about the dentist appointment]") {
		t.Errorf("first turn = %q, want the goal in front of it", first)
	}
	if !strings.HasSuffix(first, "hello?") {
		t.Errorf("first turn dropped what the person said: %q", first)
	}
	if got := <-tb.sessions; got != "phone:+15550001111" {
		t.Errorf("session key = %q, want the person being called", got)
	}

	tb.post(t, "/v1/chat/completions", chatBody("yes, it is on tuesday", false), headers)
	if second := <-tb.turns; second != "yes, it is on tuesday" {
		t.Errorf("second turn = %q, want no repeated briefing", second)
	}
}

func TestBridgeLeavesInboundTurnsAlone(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.startCall(t, "CA10", "CA10", "+15550001111")
	tb.post(t, "/v1/chat/completions", chatBody("just calling to ask something", false),
		map[string]string{"X-Factor-Call-Id": "CA10"})
	if got := <-tb.turns; got != "just calling to ask something" {
		t.Errorf("inbound turn was rewritten: %q", got)
	}
}

func TestBridgeReportsAFinishedOutboundCallToItsOrigin(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.registerOutbound("out-2", "+15550003333", "confirm the booking", origin{Channel: "telegram", ChatID: "42"})
	tb.post(t, "/internal/call-event", callEvent{
		Event: "call_ended", CallID: "out-2", Status: "completed",
		Transcript: "assistant: is tuesday ok?\nuser: yes",
	}, nil)

	select {
	case msg := <-tb.inbound:
		if msg.Channel != "telegram" || msg.ChatID != "42" {
			t.Errorf("outcome went to %s:%s, want the session that asked for the call", msg.Channel, msg.ChatID)
		}
		for _, want := range []string{"+15550003333", "completed", "yes", "Report the outcome"} {
			if !strings.Contains(msg.Content, want) {
				t.Errorf("outcome %q does not mention %q", msg.Content, want)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a finished outbound call never re-entered its session")
	}
}

func TestBridgeStaysQuietAboutInboundCalls(t *testing.T) {
	tb := newTestBridge(t, nil)
	tb.startCall(t, "CA11", "CA11", "+15550001111")
	tb.post(t, "/internal/call-event", callEvent{Event: "call_ended", CallID: "CA11", Status: "completed"}, nil)

	select {
	case msg := <-tb.inbound:
		t.Errorf("an inbound call reported itself into a session: %q", msg.Content)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBridgeRejectsUnknownCallEvents(t *testing.T) {
	tb := newTestBridge(t, nil)
	if got := tb.post(t, "/internal/call-event", callEvent{Event: "call_exploded"}, nil).StatusCode; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
	req, err := http.NewRequest(http.MethodPost, tb.url+"/internal/call-event", strings.NewReader("{"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer bridge-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed event = HTTP %d, want 400", resp.StatusCode)
	}

	unauthorized, err := http.Post(tb.url+"/internal/call-event", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated event = HTTP %d, want 401", unauthorized.StatusCode)
	}
}

func TestBridgeHealthEndpoint(t *testing.T) {
	tb := newTestBridge(t, nil)
	resp, err := http.Get(tb.url + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Errorf("health = HTTP %d %q", resp.StatusCode, body)
	}
}

// Every outcome the shell can report has to read differently, or the agent
// cannot tell the user what actually happened.
func TestOutcomeReportDistinguishesEveryStatus(t *testing.T) {
	cases := map[string]string{
		"completed": "the conversation completed",
		"no-answer": "was not answered",
		"busy":      "busy signal",
		"voicemail": "reached voicemail",
		"failed":    "failed to connect",
		"rejected":  "not on the outbound allowlist",
		"weird":     `ended with status "weird"`,
	}
	seen := map[string]bool{}
	for status, want := range cases {
		got := outcomeReport("+15550003333", status, "")
		if !strings.Contains(got, want) {
			t.Errorf("status %q produced %q, want it to mention %q", status, got, want)
		}
		if seen[got] {
			t.Errorf("status %q is indistinguishable from another outcome", status)
		}
		seen[got] = true
	}
}

func TestOutcomeReportBoundsTheTranscript(t *testing.T) {
	// Multi-byte on purpose: cutting a Spanish transcript by bytes would put
	// invalid UTF-8 in front of the model.
	long := strings.Repeat("¿qué tal? ", transcriptTailLimit)
	report := outcomeReport("+15550003333", "completed", long)
	if utf8.RuneCountInString(report) > transcriptTailLimit+400 {
		t.Errorf("report is %d runes; an unbounded transcript would flood the next turn",
			utf8.RuneCountInString(report))
	}
	if !utf8.ValidString(report) {
		t.Error("truncation sliced through a multi-byte character")
	}
	if !strings.Contains(report, "…") {
		t.Error("a truncated transcript should say so")
	}
	if strings.Contains(outcomeReport("+1555", "completed", "   "), "Transcript tail") {
		t.Error("an empty transcript should not get a heading")
	}
}

func TestBridgeListenReportsAPortClash(t *testing.T) {
	first := newBridge("t", nil, nil)
	if err := first.listen(0); err != nil {
		t.Fatal(err)
	}
	defer first.shutdown()
	first.serve()

	second := newBridge("t", nil, nil)
	if err := second.listen(first.port()); err == nil {
		second.shutdown()
		t.Fatal("a second bridge bound a port that was already taken")
	}
	if second.port() != 0 {
		t.Error("a bridge that never bound reported a port")
	}
	// Shutting down a bridge that never listened must not panic.
	second.shutdown()
}

// A turn that stops to use tools must not be dead air: what the agent says
// on its way to the answer reaches the caller while the turn is still
// running, as an ordinary content delta ahead of the reply.
func TestBridgeStreamsAMidTurnNoteBeforeTheAnswer(t *testing.T) {
	heard := make(chan struct{})
	tb := newTestBridge(t, func(_ context.Context, _, _ string, notice func(string)) (string, error) {
		notice("one moment, checking that")
		// The note is only useful if it lands before the answer does; a
		// bridge that held it back would deadlock here, so it fails instead.
		select {
		case <-heard:
		case <-time.After(10 * time.Second):
			return "", fmt.Errorf("the note never reached the caller during the turn")
		}
		return "it is on Thursday", nil
	})
	tb.startCall(t, "CA9", "CA9", "+15550001111")

	resp := tb.post(t, "/v1/chat/completions", chatBody("when is my meeting", true),
		map[string]string{"X-Factor-Call-Id": sessionIDPrefix + "CA9"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var spoken []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		payload, found := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data: ")
		if !found || payload == "[DONE]" {
			continue
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk %q is not JSON: %v", payload, err)
		}
		if text := strings.TrimSpace(chunk.Choices[0].Delta.Content); text != "" {
			spoken = append(spoken, text)
			if len(spoken) == 1 {
				close(heard) // proof it arrived without waiting for the reply
			}
		}
	}
	want := []string{"one moment, checking that", "it is on Thursday"}
	if len(spoken) != len(want) || spoken[0] != want[0] || spoken[1] != want[1] {
		t.Errorf("spoken = %q, want %q", spoken, want)
	}
}

// A turn that says something and then answers with nothing has already
// spoken: the placeholder that keeps a silent turn from being an empty
// completion would be read out on top of it.
func TestBridgeDoesNotPadAnAnswerlessTurnThatAlreadySpoke(t *testing.T) {
	tb := newTestBridge(t, func(_ context.Context, _, _ string, notice func(string)) (string, error) {
		notice("on it")
		return "", nil
	})
	tb.startCall(t, "CA10", "CA10", "+15550001111")

	resp := tb.post(t, "/v1/chat/completions", chatBody("do the thing", true),
		map[string]string{"X-Factor-Call-Id": sessionIDPrefix + "CA10"})
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "…") {
		t.Errorf("a turn that already spoke was padded: %s", body)
	}
}
