package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
)

// secretToken is deliberately distinctive so leak assertions cannot pass by
// accident.
const secretToken = "123456:SUPER-SECRET-BOT-TOKEN"

// newTelegram builds a Telegram pointed at a throwaway HTTP server.
func newTelegram(t *testing.T, h http.HandlerFunc, allow ...string) (*Telegram, *bus.MessageBus) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	b := bus.New()
	tg, err := New(Config{Token: secretToken, APIBase: srv.URL, AllowFrom: allow}, b)
	if err != nil {
		t.Fatal(err)
	}
	return tg, b
}

// newUnreachableTelegram points a Telegram at an address nothing listens on,
// so every request fails in transport.
func newUnreachableTelegram(t *testing.T) *Telegram {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // free the port before any request is made
	tg, err := New(Config{Token: secretToken, APIBase: base}, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func mustUpdate(t *testing.T, raw string) update {
	t.Helper()
	var u update
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("bad update fixture %s: %v", raw, err)
	}
	return u
}

func TestNameAndMaxMessageLength(t *testing.T) {
	tg, _ := newTelegram(t, func(http.ResponseWriter, *http.Request) {})
	if tg.Name() != "telegram" {
		t.Errorf("Name() = %q, want telegram", tg.Name())
	}
	if tg.MaxMessageLength() != 4000 {
		t.Errorf("MaxMessageLength() = %d, want 4000", tg.MaxMessageLength())
	}
}

func TestNewDefaultsToThePublicAPIBase(t *testing.T) {
	tg, err := New(Config{Token: "T"}, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://api.telegram.org/botT"; tg.apiBase != want {
		t.Errorf("apiBase = %q, want %q", tg.apiBase, want)
	}
}

func TestRedactReplacesTheTokenAndPassesNilThrough(t *testing.T) {
	tg, _ := newTelegram(t, func(http.ResponseWriter, *http.Request) {})

	if got := tg.redact(nil); got != nil {
		t.Errorf("redact(nil) = %v, want nil", got)
	}

	leaky := errors.New(`Get "https://api.telegram.org/bot` + secretToken + `/getUpdates": dial failed`)
	got := tg.redact(leaky)
	if got == nil {
		t.Fatal("redact returned nil for a non-nil error")
	}
	if strings.Contains(got.Error(), secretToken) {
		t.Errorf("redacted error still contains the bot token: %q", got.Error())
	}
	if !strings.Contains(got.Error(), "[token]") {
		t.Errorf("redacted error = %q, want it to contain [token]", got.Error())
	}
}

func TestGetUpdatesReturnsTheAPIDescriptionOnAnErrorStatus(t *testing.T) {
	tg, _ := newTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized: bot token is invalid"}`))
	})

	_, err := tg.getUpdates(context.Background())
	if err == nil {
		t.Fatal("getUpdates accepted an ok:false response")
	}
	if !strings.Contains(err.Error(), "Unauthorized: bot token is invalid") {
		t.Errorf("error = %q, want it to carry the API description", err)
	}
}

func TestGetUpdatesRejectsMalformedJSON(t *testing.T) {
	tg, _ := newTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>gateway timeout</html>`))
	})

	if _, err := tg.getUpdates(context.Background()); err == nil {
		t.Fatal("getUpdates accepted a non-JSON response body")
	}
}

func TestGetUpdatesTransportErrorNeverLeaksTheToken(t *testing.T) {
	tg := newUnreachableTelegram(t)

	_, err := tg.getUpdates(context.Background())
	if err == nil {
		t.Fatal("getUpdates succeeded against an unreachable server")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("transport error leaks the bot token: %q", err)
	}
	if !strings.Contains(err.Error(), "[token]") {
		t.Errorf("error = %q, want the token replaced by [token]", err)
	}
}

func TestGetUpdatesFailsOnATruncatedResponseBody(t *testing.T) {
	tg, _ := newTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(`{"ok":true,`)) // far short of the declared length
	})

	if _, err := tg.getUpdates(context.Background()); err == nil {
		t.Error("getUpdates accepted a body truncated mid-transfer")
	}
}

func TestGetUpdatesFailsOnAnUnparseableAPIBase(t *testing.T) {
	tg, err := New(Config{Token: secretToken, APIBase: "http://%zz"}, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.getUpdates(context.Background()); err == nil {
		t.Error("getUpdates accepted an unparseable API base")
	}
}

func TestSendFailsWhenTheAPIRejectsTheMessage(t *testing.T) {
	tg, _ := newTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	})

	err := tg.Send(context.Background(), bus.OutboundMessage{Channel: "telegram", ChatID: "1", Content: "hi"})
	if err == nil {
		t.Fatal("Send accepted an ok:false response")
	}
	if !strings.Contains(err.Error(), "chat not found") || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error = %q, want it to report HTTP 400 and the description", err)
	}
}

func TestSendFallsBackToPlainTextWhenTheHTMLIsRejected(t *testing.T) {
	var mu sync.Mutex
	var calls []map[string]any
	tg, _ := newTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, body)
		mu.Unlock()
		if _, formatted := body["parse_mode"]; formatted {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	err := tg.Send(context.Background(), bus.OutboundMessage{ChatID: "1", Content: "**broken**"})
	if err != nil {
		t.Fatalf("Send = %v, want the plain-text fallback to deliver", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("made %d sendMessage calls, want a formatted attempt then a plain one", len(calls))
	}
	if _, formatted := calls[1]["parse_mode"]; formatted || calls[1]["text"] != "**broken**" {
		t.Errorf("fallback call = %+v, want the raw text with no parse_mode", calls[1])
	}
}

func TestSendDoesNotFallBackOnANonFormatRejection(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	tg, _ := newTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests: retry after 5"}`))
	})

	if err := tg.Send(context.Background(), bus.OutboundMessage{ChatID: "1", Content: "hi"}); err == nil {
		t.Fatal("Send accepted a rate-limited response")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("made %d sendMessage calls, want the rate limit left to the manager's retry", calls)
	}
}

func TestSendTransportErrorNeverLeaksTheToken(t *testing.T) {
	tg := newUnreachableTelegram(t)

	err := tg.Send(context.Background(), bus.OutboundMessage{Channel: "telegram", ChatID: "1", Content: "hi"})
	if err == nil {
		t.Fatal("Send succeeded against an unreachable server")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("transport error leaks the bot token: %q", err)
	}
}

func TestSendFailsOnAnUnparseableAPIBase(t *testing.T) {
	tg, err := New(Config{Token: secretToken, APIBase: "http://%zz"}, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.Send(context.Background(), bus.OutboundMessage{ChatID: "1", Content: "hi"}); err == nil {
		t.Error("Send accepted an unparseable API base")
	}
}

func TestAllowedPermitsEveryoneWhenTheAllowlistIsEmpty(t *testing.T) {
	tg, _ := newTelegram(t, func(http.ResponseWriter, *http.Request) {})
	if !tg.allowed(999, "stranger") {
		t.Error("an empty allow_from must allow every sender")
	}
	if !tg.allowed(0, "") {
		t.Error("an empty allow_from must allow a sender with no username")
	}
}

func TestHandleIgnoresUpdatesWithoutUsableMessages(t *testing.T) {
	tg, b := newTelegram(t, func(http.ResponseWriter, *http.Request) {})

	ignored := map[string]string{
		"no message":     `{"update_id":1}`,
		"empty text":     `{"update_id":2,"message":{"from":{"id":5,"username":"u"},"chat":{"id":5},"text":""}}`,
		"no from":        `{"update_id":3,"message":{"chat":{"id":5},"text":"hi"}}`,
		"edit-only body": `{"update_id":4,"message":{"from":{"id":5},"chat":{"id":5}}}`,
	}
	for name, raw := range ignored {
		tg.handle(mustUpdate(t, raw))
		if n := len(b.Inbound()); n != 0 {
			t.Fatalf("%s: published %d inbound messages, want 0", name, n)
		}
	}

	tg.handle(mustUpdate(t, `{"update_id":5,"message":{"from":{"id":42,"username":"nico"},"chat":{"id":77},"text":"hello"}}`))
	select {
	case msg := <-b.Inbound():
		if msg.SenderID != "42" || msg.ChatID != "77" || msg.Content != "hello" {
			t.Errorf("published %+v, want sender 42, chat 77, content hello", msg)
		}
	default:
		t.Error("a complete message was not published")
	}
}

func TestSleepCtxReturnsImmediatelyOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Hour)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sleepCtx waited %v on a cancelled context, want an immediate return", elapsed)
	}
}

func TestSleepCtxReturnsWhenTheTimerFires(t *testing.T) {
	start := time.Now()
	sleepCtx(context.Background(), time.Millisecond)
	if elapsed := time.Since(start); elapsed < time.Millisecond {
		t.Errorf("sleepCtx returned after %v, want at least 1ms", elapsed)
	}
}

func TestPollLoopKeepsPollingAfterAFailedPoll(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	tg, _ := newTelegram(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		polls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":false,"description":"flood wait"}`))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Start(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := polls
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poll loop never issued a request")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stop must interrupt the 5s error backoff rather than wait it out.
	stopped := make(chan struct{})
	go func() {
		_ = tg.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not interrupt the poll-failure backoff")
	}
}

func TestPollLoopExitsWhenTheContextIsCancelledMidRequest(t *testing.T) {
	entered := make(chan struct{}, 1)
	tg, _ := newTelegram(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done() // hold the request open until the poller gives up
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the poll loop never issued a request")
	}

	stopped := make(chan struct{})
	go func() {
		_ = tg.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not unblock the in-flight poll request")
	}
}

// fastTyping shortens the chat-action refresh so a test can watch it repeat.
func fastTyping(t *testing.T) {
	t.Helper()
	original := typingInterval
	typingInterval = 10 * time.Millisecond
	t.Cleanup(func() { typingInterval = original })
}

// typingServer answers polls slowly and counts typing actions per chat.
func typingServer(t *testing.T) (*Telegram, func(chatID string) int) {
	t.Helper()
	var mu sync.Mutex
	actions := map[string]int{}
	tg, _ := newTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendChatAction") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["action"] != "typing" {
				t.Errorf("chat action = %v, want typing", body["action"])
			}
			mu.Lock()
			actions[body["chat_id"].(string)]++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		time.Sleep(20 * time.Millisecond) // a hot poll loop would drown the counts
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	return tg, func(chatID string) int {
		mu.Lock()
		defer mu.Unlock()
		return actions[chatID]
	}
}

// waitForActions polls until chatID has at least want actions.
func waitForActions(t *testing.T, count func(string) int, chatID string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for count(chatID) < want {
		if time.Now().After(deadline) {
			t.Fatalf("chat %s got %d typing actions, want at least %d", chatID, count(chatID), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSetTypingRepeatsTheActionUntilItIsStopped(t *testing.T) {
	fastTyping(t)

	tg, count := typingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tg.Stop() }()

	tg.SetTyping("77", true)
	waitForActions(t, count, "77", 3) // the indicator has to be refreshed, not sent once

	tg.SetTyping("77", false)
	settled := count("77")
	time.Sleep(100 * time.Millisecond)
	if after := count("77"); after > settled+1 { // one action may already be in flight
		t.Errorf("typing actions grew from %d to %d after the turn ended", settled, after)
	}
}

func TestSetTypingStartsOneLoopPerChat(t *testing.T) {
	tg, _ := typingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tg.Stop() }()

	tg.SetTyping("77", true)
	tg.SetTyping("77", true)
	tg.SetTyping("88", true)

	tg.typingMu.Lock()
	live := len(tg.typing)
	tg.typingMu.Unlock()
	if live != 2 {
		t.Errorf("live typing loops = %d, want one per chat (2)", live)
	}
}

func TestSetTypingIsInertWithoutAStartedConnector(t *testing.T) {
	tg, count := typingServer(t)

	tg.SetTyping("77", true)  // no run context yet: nothing to hang the loop off
	tg.SetTyping("77", false) // stopping a chat that was never typing

	tg.typingMu.Lock()
	live := len(tg.typing)
	tg.typingMu.Unlock()
	if live != 0 {
		t.Errorf("live typing loops = %d before Start, want 0", live)
	}
	time.Sleep(50 * time.Millisecond)
	if n := count("77"); n != 0 {
		t.Errorf("sent %d typing actions before Start, want 0", n)
	}
}

func TestStopEndsTypingLoops(t *testing.T) {
	fastTyping(t)

	tg, count := typingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tg.SetTyping("77", true)
	waitForActions(t, count, "77", 1)

	stopped := make(chan struct{})
	go func() {
		_ = tg.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung on a running typing loop")
	}

	settled := count("77")
	tg.SetTyping("88", true) // a turn still finishing after shutdown
	time.Sleep(100 * time.Millisecond)
	if after := count("77"); after != settled {
		t.Errorf("typing actions grew from %d to %d after Stop", settled, after)
	}
	if n := count("88"); n != 0 {
		t.Errorf("a stopped connector sent %d typing actions, want 0", n)
	}
}

func TestTypingLoopSurvivesARejectedAction(t *testing.T) {
	fastTyping(t)

	var mu sync.Mutex
	calls := 0
	tg, _ := newTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendChatAction") {
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests"}`))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tg.Stop() }()

	tg.SetTyping("77", true)
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the typing loop stopped after %d rejected actions, want it to keep trying", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopIsSafeBeforeStart(t *testing.T) {
	tg, _ := newTelegram(t, func(http.ResponseWriter, *http.Request) {})
	if err := tg.Stop(); err != nil {
		t.Errorf("Stop before Start = %v, want nil", err)
	}
}

func TestRegisteredFactoryBuildsTelegramFromAConfigSection(t *testing.T) {
	built := channel.Build(map[string]json.RawMessage{
		"telegram": json.RawMessage(`{"token":"` + secretToken + `","allow_from":["111"]}`),
	}, bus.New())

	if len(built) != 1 {
		t.Fatalf("Build() = %v, want one telegram channel", built)
	}
	if built[0].Name() != "telegram" {
		t.Errorf("built channel = %q, want telegram", built[0].Name())
	}
}

func TestRegisteredFactoryRejectsBadTelegramSections(t *testing.T) {
	tests := map[string]json.RawMessage{
		"empty token":    json.RawMessage(`{"token":""}`),
		"missing token":  json.RawMessage(`{"allow_from":["111"]}`),
		"malformed json": json.RawMessage(`{"token":`),
		"wrong type":     json.RawMessage(`{"token":12345}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if built := channel.Build(map[string]json.RawMessage{"telegram": raw}, bus.New()); len(built) != 0 {
				t.Errorf("Build() = %v, want the invalid section skipped", built)
			}
		})
	}
}
