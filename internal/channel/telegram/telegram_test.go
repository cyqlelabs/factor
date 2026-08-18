package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
)

type fakeAPI struct {
	mu      sync.Mutex
	sent    []map[string]any
	updates string
	served  bool
	offsets []string
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/botTOKEN/getUpdates":
			f.offsets = append(f.offsets, r.URL.Query().Get("offset"))
			if f.served {
				time.Sleep(50 * time.Millisecond)
				fmt.Fprint(w, `{"ok":true,"result":[]}`)
				return
			}
			f.served = true
			fmt.Fprint(w, f.updates)
		case "/botTOKEN/sendMessage":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.sent = append(f.sent, body)
			fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}
}

const updatesFixture = `{"ok":true,"result":[
	{"update_id":10,"message":{"from":{"id":111,"username":"nico"},"chat":{"id":111},"text":"hello agent"}},
	{"update_id":11,"message":{"from":{"id":999,"username":"stranger"},"chat":{"id":999},"text":"let me in"}}
]}`

func newTestTelegram(t *testing.T, allow []string) (*Telegram, *fakeAPI, *bus.MessageBus) {
	t.Helper()
	api := &fakeAPI{updates: updatesFixture}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	b := bus.New()
	tg, err := New(Config{Token: "TOKEN", AllowFrom: allow, APIBase: srv.URL}, b)
	if err != nil {
		t.Fatal(err)
	}
	return tg, api, b
}

func TestPollPublishesAllowedOnly(t *testing.T) {
	tg, api, b := newTestTelegram(t, []string{"111"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tg.Stop() }()

	select {
	case msg := <-b.Inbound():
		if msg.Channel != "telegram" || msg.SenderID != "111" || msg.Content != "hello agent" {
			t.Errorf("msg = %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message")
	}
	// the stranger's message must NOT arrive
	select {
	case msg := <-b.Inbound():
		t.Errorf("non-allowed sender leaked through: %+v", msg)
	case <-time.After(200 * time.Millisecond):
	}
	// offset advanced past both updates
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		api.mu.Lock()
		n := len(api.offsets)
		last := ""
		if n > 0 {
			last = api.offsets[n-1]
		}
		api.mu.Unlock()
		if last == "12" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("offset never advanced to 12")
}

func TestUsernameAllowlist(t *testing.T) {
	tg, _, _ := newTestTelegram(t, []string{"@nico"})
	if !tg.allowed(111, "nico") {
		t.Error("@nico should be allowed")
	}
	if tg.allowed(999, "stranger") {
		t.Error("stranger should be denied")
	}
}

func TestSend(t *testing.T) {
	tg, api, _ := newTestTelegram(t, nil)
	err := tg.Send(context.Background(), bus.OutboundMessage{Channel: "telegram", ChatID: "111", Content: "reply text"})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 1 || api.sent[0]["text"] != "reply text" || api.sent[0]["chat_id"] != "111" {
		t.Errorf("sent = %+v", api.sent)
	}
}

func TestSendFormatsMarkdownAsTelegramHTML(t *testing.T) {
	tg, api, _ := newTestTelegram(t, nil)
	err := tg.Send(context.Background(), bus.OutboundMessage{Channel: "telegram", ChatID: "111", Content: "**bold** and `code`"})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 1 {
		t.Fatalf("sent = %+v, want one message", api.sent)
	}
	if api.sent[0]["text"] != "<b>bold</b> and <code>code</code>" || api.sent[0]["parse_mode"] != "HTML" {
		t.Errorf("sent = %+v, want Telegram HTML with parse_mode HTML", api.sent[0])
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(Config{}, bus.New()); err == nil {
		t.Error("empty token accepted")
	}
}
