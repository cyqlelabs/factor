package rendezvous

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/swarm"
	"github.com/cyqlelabs/factor/internal/swarm/wire"
)

type quietTurns struct{}

func (quietTurns) ProcessRemote(context.Context, agent.RemoteTurn) (string, error) { return "ok", nil }
func (quietTurns) AudienceAt(string, string) string                                { return "" }

func TestPairingThroughTheMailbox(t *testing.T) {
	r := newRig(t)
	start := func(name string) *swarm.Service {
		svc, err := swarm.New(config.SwarmConfig{Enabled: true, Name: name, Rendezvous: r.url, RemoteTasks: 1},
			t.TempDir(), "test", quietTurns{}, bus.New())
		if err != nil {
			t.Fatal(err)
		}
		svc.Connect()
		t.Cleanup(svc.Close)
		return svc
	}
	desk, home := start("desk"), start("home")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	codes := make(chan string, 1)
	done := make(chan error, 1)
	if err := desk.Invite(ctx, "", func(code string) error { codes <- code; return nil },
		func(_ string, err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	if _, err := home.Join(ctx, <-codes, ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("invite: %v", err)
	}
	if !desk.IsMember(home.ID()) {
		t.Error("not paired")
	}
}

// mailClient speaks the mailbox protocol by hand.
type mailClient struct {
	t  *testing.T
	ws *websocket.Conn
}

func (r *rig) mailbox(t *testing.T) *mailClient {
	t.Helper()
	resp, err := http.Get(strings.Replace(r.url, "ws", "http", 1) + "/v1/work")
	if err != nil {
		t.Fatal(err)
	}
	var ch struct {
		Nonce string `json:"nonce"`
		Bits  int    `json:"bits"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ch)
	resp.Body.Close()
	work, _ := wire.SolveWork(ch.Nonce, "mailbox", ch.Bits)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, r.url+"/v1/mailbox?work="+url.QueryEscape(ch.Nonce+"~"+work), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	c := &mailClient{t: t, ws: ws}
	if m := c.read(); m["type"] != "welcome" {
		t.Fatalf("first message %v", m)
	}
	return c
}

func (c *mailClient) say(m map[string]any) {
	data, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		c.t.Fatal(err)
	}
}

func (c *mailClient) read() map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return map[string]any{"type": "closed-socket", "error": err.Error()}
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

// reply skips acks to the next real answer.
func (c *mailClient) reply() map[string]any {
	for {
		if m := c.read(); m["type"] != "ack" {
			return m
		}
	}
}

func TestMailboxProtocol(t *testing.T) {
	r := newRig(t)
	if resp, err := http.Get(strings.Replace(r.url, "ws", "http", 1) + "/v1/mailbox"); err == nil {
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("no proof of work: %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	a := r.mailbox(t)
	a.say(map[string]any{"type": "allocate", "id": "1"})
	if m := a.reply(); m["type"] != "error" {
		t.Errorf("allocate before bind: %v", m)
	}

	a = r.mailbox(t)
	a.say(map[string]any{"type": "bind", "id": "1", "side": "a", "appid": "app"})
	a.say(map[string]any{"type": "list", "id": "2"})
	if m := a.reply(); m["type"] != "nameplates" || len(m["nameplates"].([]any)) != 0 {
		t.Errorf("list: %v", m)
	}
	a.say(map[string]any{"type": "add", "id": "3", "phase": "x", "body": "y"})
	if m := a.reply(); m["type"] != "error" {
		t.Errorf("add before open: %v", m)
	}

	a = r.mailbox(t)
	a.say(map[string]any{"type": "bind", "id": "1", "side": "a", "appid": "app"})
	a.say(map[string]any{"type": "allocate", "id": "2"})
	alloc := a.reply()
	if alloc["type"] != "allocated" || alloc["nameplate"] != "1" {
		t.Fatalf("allocate: %v", alloc)
	}
	a.say(map[string]any{"type": "claim", "id": "3", "nameplate": "1"})
	claimed := a.reply()
	a.say(map[string]any{"type": "open", "id": "4", "mailbox": claimed["mailbox"]})
	a.say(map[string]any{"type": "add", "id": "5", "phase": "pake", "body": "aa"})
	if m := a.reply(); m["type"] != "message" || m["body"] != "aa" {
		t.Errorf("own message echoed: %v", m)
	}

	b := r.mailbox(t)
	b.say(map[string]any{"type": "bind", "id": "1", "side": "b", "appid": "app"})
	b.say(map[string]any{"type": "claim", "id": "2", "nameplate": "9"})
	if m := b.reply(); m["type"] != "error" {
		t.Errorf("claim of a code nobody made: %v", m)
	}
	b.say(map[string]any{"type": "claim", "id": "3", "nameplate": "1"})
	if m := b.reply(); m["type"] != "claimed" || m["mailbox"] != claimed["mailbox"] {
		t.Fatalf("claim: %v", m)
	}
	b.say(map[string]any{"type": "open", "id": "4", "mailbox": claimed["mailbox"]})
	if m := b.reply(); m["type"] != "message" || m["side"] != "a" {
		t.Errorf("backlog: %v", m)
	}
	// A third side is crowded out.
	c := r.mailbox(t)
	c.say(map[string]any{"type": "bind", "id": "1", "side": "c", "appid": "app"})
	c.say(map[string]any{"type": "claim", "id": "2", "nameplate": "1"})
	if m := c.reply(); m["type"] != "error" || m["error"] != "crowded" {
		t.Errorf("third side: %v", m)
	}
	// Three misses on one connection end it.
	for i := range 3 {
		c.say(map[string]any{"type": "claim", "id": "x", "nameplate": "99"})
		_ = i
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m := c.read(); m["type"] == "closed-socket" {
			break
		}
	}
	// A scary close burns the code: nobody new can claim it.
	b.say(map[string]any{"type": "close", "id": "5", "mailbox": claimed["mailbox"], "mood": "scary"})
	if m := b.reply(); m["type"] != "closed" {
		t.Errorf("close: %v", m)
	}
	d := r.mailbox(t)
	d.say(map[string]any{"type": "bind", "id": "1", "side": "d", "appid": "app"})
	d.say(map[string]any{"type": "claim", "id": "2", "nameplate": "1"})
	if m := d.reply(); m["type"] != "error" {
		t.Errorf("claim of a burned code: %v", m)
	}
	a.say(map[string]any{"type": "release", "id": "6", "nameplate": "1"})
	if m := a.reply(); m["type"] != "released" {
		t.Errorf("release: %v", m)
	}
	a.say(map[string]any{"type": "close", "id": "7", "mailbox": claimed["mailbox"], "mood": "happy"})
	if m := a.reply(); m["type"] != "closed" {
		t.Errorf("last close: %v", m)
	}
	a.say(map[string]any{"type": "dance", "id": "8"})
	if m := a.reply(); m["type"] != "error" {
		t.Errorf("unknown: %v", m)
	}
}

func TestMailboxWorkAndExpiry(t *testing.T) {
	r := newRig(t)
	m := r.srv.mail
	if m.spend("junk") || m.spend("a.b.c~1") || m.spend("1.2~3") {
		t.Error("a forged work token was accepted")
	}
	nonce := m.nonce(time.Now())
	work, _ := wire.SolveWork(nonce, "mailbox", r.srv.cfg.WorkBits)
	if !m.spend(nonce+"~"+work) || m.spend(nonce+"~"+work) {
		t.Error("a work token is single use")
	}
	old := m.nonce(time.Now().Add(-time.Hour))
	work, _ = wire.SolveWork(old, "mailbox", r.srv.cfg.WorkBits)
	if m.spend(old + "~" + work) {
		t.Error("a stale work token was accepted")
	}
	a := r.mailbox(t)
	a.say(map[string]any{"type": "bind", "id": "1", "side": "a", "appid": "app"})
	a.say(map[string]any{"type": "allocate", "id": "2"})
	a.reply()
	r.srv.now = func() time.Time { return time.Now().Add(time.Hour) }
	m.mu.Lock()
	m.expireLocked(r.srv.now())
	left := len(m.byID)
	m.mu.Unlock()
	if left != 0 {
		t.Error("a mailbox outlived its lifetime")
	}
}
