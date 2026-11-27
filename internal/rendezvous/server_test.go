package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/cyqlelabs/factor/internal/swarm"
)

// online starts a link for id in swarm (or none) and waits for it.
type rig struct {
	t   *testing.T
	srv *Server
	url string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	srv, err := New(Config{Data: filepath.Join(t.TempDir(), "store"), Create: true, WorkBits: 4})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &rig{t: t, srv: srv, url: "ws" + strings.TrimPrefix(hs.URL, "http")}
}

func (r *rig) online(t *testing.T, id *swarm.Identity, swarmID string) *swarm.Link {
	t.Helper()
	l := swarm.NewLink(r.url, id, func() string { return swarmID })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	wctx, done := context.WithTimeout(ctx, 5*time.Second)
	defer done()
	if err := l.WaitOnline(wctx); err != nil {
		state, _ := l.Status()
		t.Fatalf("link never came up: %s", state)
	}
	return l
}

// pairOf commits a roster holding a and b, and brings both online on it.
func pairOf(t *testing.T, r *rig) (a, b *swarm.Identity, la, lb *swarm.Link) {
	t.Helper()
	a, _ = swarm.NewIdentity()
	b, _ = swarm.NewIdentity()
	g := swarm.Genesis(a, swarm.NewSwarmID(), r.url)
	two := g.Next(a, func(x *swarm.Roster) { x.Add(swarm.EncodeKey(b.Public), swarm.RoleMember) })
	if err := r.srv.store.Commit(g); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.store.Commit(two); err != nil {
		t.Fatal(err)
	}
	return a, b, r.online(t, a, g.Swarm), r.online(t, b, g.Swarm)
}

func TestStreamCarriesBulkWithFlowControl(t *testing.T) {
	r := newRig(t)
	r.srv.cfg.RateBytes, r.srv.cfg.BurstBytes = 1<<30, 1<<30
	_, b, la, lb := pairOf(t, r)
	got := make(chan []byte, 1)
	lb.OnAccept(func(c net.Conn, from string) {
		data, _ := io.ReadAll(c)
		got <- data
		_ = c.Close()
	})
	conn, err := la.Dial(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("swarm!", 200_000)) // past several windows
	if n, err := conn.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("wrote %d: %v", n, err)
	}
	_ = conn.Close()
	select {
	case data := <-got:
		if len(data) != len(payload) {
			t.Fatalf("received %d of %d bytes", len(data), len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bytes never arrived")
	}
	if conn.RemoteAddr().String() != b.ID || conn.LocalAddr().Network() != "swarm" {
		t.Error("addresses")
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("wrote to a closed stream")
	}
}

func TestStreamDeadlinesAndRemoteClose(t *testing.T) {
	r := newRig(t)
	_, b, la, lb := pairOf(t, r)
	accepted := make(chan net.Conn, 1)
	lb.OnAccept(func(c net.Conn, _ string) { accepted <- c })
	conn, err := la.Dial(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := conn.Read(make([]byte, 4)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read past the deadline: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
	remote := <-accepted
	_, _ = remote.Write([]byte("hi"))
	buf := make([]byte, 4)
	if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "hi" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	_ = remote.Close()
	if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
		t.Errorf("after the far side closed: %v", err)
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("wrote to a stream the far side closed")
	}
}

func TestRelayRefusesOutsidersAndTheOffline(t *testing.T) {
	r := newRig(t)
	_, b, la, _ := pairOf(t, r)
	stranger, _ := swarm.NewIdentity()
	ls := r.online(t, stranger, "")
	if _, err := ls.Dial(context.Background(), b.ID); err == nil || !strings.Contains(err.Error(), "not on a roster") {
		t.Errorf("a key on no roster relayed: %v", err)
	}
	if _, err := la.Dial(context.Background(), stranger.ID); err == nil {
		t.Error("relayed to a key outside the swarm")
	}
	offline, _ := swarm.NewIdentity()
	if _, err := la.Dial(context.Background(), offline.ID); err == nil {
		t.Error("relayed to an agent that is not online")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	online, err := la.Presence(ctx)
	if err != nil || len(online) != 2 {
		t.Errorf("presence %v %v", online, err)
	}
	if r, err := la.Fetch(ctx, ""); err != nil || r.Epoch != 2 {
		t.Errorf("fetch %v %v", r, err)
	}
	if _, err := ls.Fetch(ctx, ""); err == nil {
		t.Error("a stranger fetched a roster")
	}
	if _, err := la.Request(ctx, swarm.Control{Type: "dance"}); err == nil {
		t.Error("an unknown request was answered")
	}
}

func TestDailyQuotaEndsTheStream(t *testing.T) {
	r := newRig(t)
	r.srv.cfg.DailyBytes = 1000
	_, b, la, lb := pairOf(t, r)
	lb.OnAccept(func(c net.Conn, _ string) { _, _ = io.Copy(io.Discard, c) })
	conn, err := la.Dial(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write(make([]byte, 4000))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("past the daily quota: %v", err)
	}
}

func TestHandshakeRefusals(t *testing.T) {
	r := newRig(t)
	r.srv.cfg.WorkBits = 30 // more than a test will ever pay
	id, _ := swarm.NewIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, r.url+"/v1/agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	var ch swarm.Control
	_, data, _ := ws.Read(ctx)
	_ = json.Unmarshal(data, &ch)
	// A signature over the wrong nonce.
	hello, _ := json.Marshal(swarm.Control{Type: swarm.CtlHello, Key: swarm.EncodeKey(id.Public), Sig: id.SignHello("other")})
	_ = ws.Write(ctx, websocket.MessageText, hello)
	if _, _, err := ws.Read(ctx); err == nil {
		t.Error("a forged hello was welcomed")
	}
	// A new key that skips the work.
	l := swarm.NewLink(r.url, id, func() string { return "" })
	go func() {
		l.Run(ctx)
	}()
	if state := stateWith(l, "proof of work"); l.Online() || !strings.Contains(state, "proof of work") {
		t.Errorf("a new key against a greedy rendezvous: %q", state)
	}
	// And one that pays nothing is not welcomed.
	r.srv.cfg.WorkBits = 8
	ws2, _, err := websocket.Dial(ctx, r.url+"/v1/agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, data, _ = ws2.Read(ctx)
	_ = json.Unmarshal(data, &ch)
	hello, _ = json.Marshal(swarm.Control{Type: swarm.CtlHello, Key: swarm.EncodeKey(id.Public), Sig: id.SignHello(ch.Nonce)})
	_ = ws2.Write(ctx, websocket.MessageText, hello)
	if _, _, err := ws2.Read(ctx); err == nil {
		t.Error("a new key that skipped the work was welcomed")
	}

	// A removed member is refused outright.
	a, _ := swarm.NewIdentity()
	g := swarm.Genesis(a, swarm.NewSwarmID(), r.url)
	_ = r.srv.store.Commit(g)
	two := g.Next(a, func(x *swarm.Roster) { x.Add(swarm.EncodeKey(id.Public), swarm.RoleMember) })
	_ = r.srv.store.Commit(two)
	three := two.Next(a, func(x *swarm.Roster) { x.Remove(swarm.EncodeKey(id.Public)) })
	_ = r.srv.store.Commit(three)
	gone := swarm.NewLink(r.url, id, func() string { return g.Swarm })
	go gone.Run(ctx)
	if state := stateWith(gone, "removed"); gone.Online() || !strings.Contains(state, "removed") {
		t.Errorf("a removed agent: %q", state)
	}
}

// stateWith waits for a link's state to mention want, and returns the state
// it ended on.
func stateWith(l *swarm.Link, want string) string {
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, _ := l.Status()
		if strings.Contains(state, want) || time.Now().After(deadline) {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommitRules(t *testing.T) {
	r := newRig(t)
	a, b, la, _ := pairOf(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	head, _ := la.Fetch(ctx, "")
	// A roster signed by someone else cannot be committed by this agent.
	byB := head.Next(b, func(*swarm.Roster) {})
	if err := la.Commit(ctx, byB); err == nil {
		t.Error("committed a roster another member signed")
	}
	stale := head.Next(a, func(*swarm.Roster) {})
	stale.Epoch = 9
	stale.Sign(a)
	if err := la.Commit(ctx, stale); err == nil || !strings.Contains(err.Error(), "epoch 2") {
		t.Errorf("a roster that does not follow the head: %v", err)
	}
}

func TestPerIPLimitAndHealth(t *testing.T) {
	r := newRig(t)
	r.srv.cfg.PerIPMinute = 1
	resp, err := http.Get(strings.Replace(r.url, "ws", "http", 1) + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if health["ok"] != true {
		t.Errorf("health %v", health)
	}
	for range 2 {
		resp, err := http.Get(strings.Replace(r.url, "ws", "http", 1) + "/v1/agent")
		if err == nil {
			resp.Body.Close()
		}
		if err == nil && resp.StatusCode == http.StatusTooManyRequests {
			return
		}
	}
	t.Error("one address opened more connections than allowed")
}

func TestClientIPAndBucket(t *testing.T) {
	s := &Server{cfg: Config{TrustProxy: true}}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	if got := s.clientIP(req); got != "5.6.7.8" {
		t.Errorf("behind a proxy: %q", got)
	}
	req.Header.Add("X-Forwarded-For", "9.9.9.9")
	if got := s.clientIP(req); got != "9.9.9.9" {
		t.Errorf("behind a proxy that adds its own line: %q", got)
	}
	s.cfg.TrustProxy = false
	if got := s.clientIP(req); got != "10.0.0.1" {
		t.Errorf("direct: %q", got)
	}
	now := time.Now()
	b := bucket{rate: 1000, burst: 1000, tokens: 1000, last: now}
	if b.take(1000, now) != 0 || b.take(500, now) < 400*time.Millisecond {
		t.Error("bucket")
	}
}

func TestBrokenStoreFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if _, err := OpenStore(dir, true); err != nil {
		t.Fatal(err)
	}
	a, _ := swarm.NewIdentity()
	g := swarm.Genesis(a, swarm.NewSwarmID(), "")
	other := swarm.Genesis(a, swarm.NewSwarmID(), "")
	_ = os.WriteFile(filepath.Join(dir, "rosters", g.Swarm+".json"), []byte("{broken"), 0o600)
	data, _ := json.Marshal(other)
	_ = os.WriteFile(filepath.Join(dir, "rosters", "aaaa.json"), data, 0o600)
	s, err := OpenStore(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(g); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("a swarm with a damaged roster changed: %v", err)
	}
	if s.Head("aaaa") != nil {
		t.Error("a file holding another swarm's roster was read as this one's")
	}
	bad := *g
	bad.Swarm = "UPPER/../x"
	if err := s.Commit(&bad); err == nil {
		t.Error("an odd swarm id was stored")
	}
}
