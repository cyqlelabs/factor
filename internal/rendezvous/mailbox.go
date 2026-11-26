package rendezvous

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/cyqlelabs/factor/internal/swarm/wire"
)

// The mailbox side of the magic-wormhole rendezvous protocol, which
// wormhole-william's client speaks. Two agents meet in a mailbox found by a
// short number — the first group of a pairing code — and run SPAKE2 through
// it; the mailbox relays their messages and can read none of them.

// maxMailboxes bounds the pairings in flight at once.
const maxMailboxes = 2000

type mailboxes struct {
	s      *Server
	secret []byte

	mu         sync.Mutex
	byName     map[string]map[int]*mailbox // app id → nameplate → mailbox
	byID       map[string]*mailbox
	spentWorks map[string]time.Time
}

type mailbox struct {
	id        string
	app       string
	nameplate int
	created   time.Time
	claims    map[string]bool
	closed    map[string]bool
	msgs      []mailMsg
	listeners map[*mailConn]bool
}

type mailMsg struct {
	Type     string  `json:"type"`
	Side     string  `json:"side"`
	Phase    string  `json:"phase"`
	Body     string  `json:"body"`
	ID       string  `json:"id"`
	ServerRX float64 `json:"server_rx"`
	ServerTX float64 `json:"server_tx"`
}

func newMailboxes(s *Server) *mailboxes {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &mailboxes{s: s, secret: secret, byName: map[string]map[int]*mailbox{},
		byID: map[string]*mailbox{}, spentWorks: map[string]time.Time{}}
}

// serveWork hands out a proof-of-work challenge for opening a mailbox. It is
// stateless: the nonce carries its own time and a MAC.
func (m *mailboxes) serveWork(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"nonce": m.nonce(m.s.now()), "bits": m.s.cfg.WorkBits})
}

func (m *mailboxes) nonce(at time.Time) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	body := strconv.FormatInt(at.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// spend checks a solved challenge and marks it used.
func (m *mailboxes) spend(token string) bool {
	nonce, work, ok := strings.Cut(token, "~")
	if !ok {
		return m.s.cfg.WorkBits == 0
	}
	parts := strings.Split(nonce, ".")
	if len(parts) != 3 {
		return false
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return false
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	now := m.s.now()
	if err != nil || now.Sub(time.Unix(ts, 0)) > 2*time.Minute {
		return false
	}
	if !wire.CheckWork(nonce, "mailbox", work, m.s.cfg.WorkBits) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, at := range m.spentWorks {
		if now.Sub(at) > 3*time.Minute {
			delete(m.spentWorks, k)
		}
	}
	if _, used := m.spentWorks[nonce]; used {
		return false
	}
	m.spentWorks[nonce] = now
	return true
}

// expire drops mailboxes past their lifetime.
func (m *mailboxes) expireLocked(now time.Time) {
	for id, mb := range m.byID {
		if now.Sub(mb.created) > m.s.cfg.MailboxTTL {
			m.deleteLocked(mb, nil)
			delete(m.byID, id)
		}
	}
}

// releaseLocked frees a mailbox's code, so nobody new can claim it.
func (m *mailboxes) releaseLocked(mb *mailbox) {
	if names := m.byName[mb.app]; names != nil && names[mb.nameplate] == mb {
		delete(names, mb.nameplate)
	}
}

// deleteLocked forgets a mailbox and ends the connections still on it, but
// for keep, which is waiting for its own reply.
func (m *mailboxes) deleteLocked(mb *mailbox, keep *mailConn) {
	delete(m.byID, mb.id)
	m.releaseLocked(mb)
	for l := range mb.listeners {
		if l != keep {
			l.drop()
		}
	}
}

type mailConn struct {
	m    *mailboxes
	ws   *websocket.Conn
	ctx  context.Context
	side string
	app  string
	box  *mailbox
	out  chan []byte
	once sync.Once
	gone chan struct{}
	// misses counts claims of codes nobody is waiting under.
	misses int
}

func (c *mailConn) drop() { c.once.Do(func() { close(c.gone) }) }

func (c *mailConn) send(v map[string]any) {
	v["server_tx"] = float64(c.m.s.now().UnixNano()) / 1e9
	data, _ := json.Marshal(v)
	select {
	case c.out <- data:
	case <-c.ctx.Done():
	case <-c.gone:
	}
}

// mailIn is one client message, with every field any message type uses.
type mailIn struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Side      string `json:"side"`
	AppID     string `json:"appid"`
	Nameplate string `json:"nameplate"`
	Mailbox   string `json:"mailbox"`
	Phase     string `json:"phase"`
	Body      string `json:"body"`
	Mood      string `json:"mood"`
}

func (m *mailboxes) serve(w http.ResponseWriter, r *http.Request) {
	if !m.s.admitIP(r) {
		http.Error(w, "too many connections from this address", http.StatusTooManyRequests)
		return
	}
	if !m.spend(r.URL.Query().Get("work")) {
		http.Error(w, "a mailbox needs a fresh proof of work from /v1/work", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 10)
	ctx, cancel := context.WithTimeout(r.Context(), m.s.cfg.MailboxTTL)
	defer cancel()
	c := &mailConn{m: m, ws: ws, ctx: ctx, out: make(chan []byte, 64), gone: make(chan struct{})}
	go c.writeLoop(cancel)
	defer c.end()
	c.send(map[string]any{"type": "welcome", "welcome": map[string]any{"motd": ""}})
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var msg mailIn
		if json.Unmarshal(data, &msg) != nil {
			return
		}
		c.send(map[string]any{"type": "ack", "id": msg.ID})
		if !c.handle(msg) {
			return
		}
	}
}

func (c *mailConn) writeLoop(cancel context.CancelFunc) {
	defer cancel()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.gone:
			_ = c.ws.Close(websocket.StatusNormalClosure, "the mailbox is closed")
			return
		case data := <-c.out:
			if c.ws.Write(c.ctx, websocket.MessageText, data) != nil {
				return
			}
		}
	}
}

// end leaves the mailbox and closes the socket once the replies queued on it
// have gone out: the last one is usually why the connection is ending, and a
// client only learns it if it leaves before the socket does.
func (c *mailConn) end() {
	c.m.mu.Lock()
	if c.box != nil {
		delete(c.box.listeners, c)
	}
	c.m.mu.Unlock()
	for wait := time.Now().Add(time.Second); len(c.out) > 0 && time.Now().Before(wait); {
		time.Sleep(5 * time.Millisecond)
	}
	_ = c.ws.CloseNow()
}

// fail says no and reports that the connection should end — the way this
// server refuses, since the client waits for a reply it would otherwise
// never get.
func (c *mailConn) fail(why string) bool {
	c.sendLocked(map[string]any{"type": "error", "error": why})
	return false
}

// handle runs one client message, reporting whether the connection goes on.
func (c *mailConn) handle(msg mailIn) bool {
	switch msg.Type {
	case "bind":
		if c.side != "" || msg.Side == "" || msg.AppID == "" {
			return c.fail("bind needs a side and an app id, once")
		}
		c.side, c.app = msg.Side, msg.AppID
		return true
	case "list":
		// Listing would let anyone enumerate the codes in flight.
		c.send(map[string]any{"type": "nameplates", "nameplates": []any{}})
		return true
	}
	if c.side == "" {
		return c.fail("bind first")
	}
	op, ok := map[string]func(mailIn) bool{
		"allocate": c.allocate, "claim": c.claim, "open": c.open,
		"add": c.add, "release": c.release, "close": c.closeBox,
	}[msg.Type]
	if !ok {
		return c.fail("unknown message " + msg.Type)
	}
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	c.m.expireLocked(c.m.s.now())
	return op(msg)
}

func (c *mailConn) allocate(mailIn) bool {
	m := c.m
	if len(m.byID) >= maxMailboxes {
		return c.fail("too many pairings in flight; try again in a few minutes")
	}
	names := m.byName[c.app]
	if names == nil {
		names = map[int]*mailbox{}
		m.byName[c.app] = names
	}
	n := 1
	for names[n] != nil {
		n++
	}
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	mb := &mailbox{id: hex.EncodeToString(raw), app: c.app, nameplate: n, created: m.s.now(),
		claims: map[string]bool{}, closed: map[string]bool{}, listeners: map[*mailConn]bool{}}
	names[n] = mb
	m.byID[mb.id] = mb
	c.sendLocked(map[string]any{"type": "allocated", "nameplate": strconv.Itoa(n)})
	return true
}

func (c *mailConn) claim(msg mailIn) bool {
	n, err := strconv.Atoi(msg.Nameplate)
	mb := c.m.byName[c.app][n]
	if err != nil || mb == nil {
		// Said, not hung up on: the client only ever waits for a reply, and
		// closing under it races its own teardown. Guessing is what several
		// misses on one connection look like, and that is cut.
		c.misses++
		c.sendLocked(map[string]any{"type": "error", "error": "no pairing is waiting under that code"})
		return c.misses < 3
	}
	if !mb.claims[c.side] && len(mb.claims) >= 2 {
		return c.fail("crowded")
	}
	mb.claims[c.side] = true
	c.sendLocked(map[string]any{"type": "claimed", "mailbox": mb.id})
	return true
}

func (c *mailConn) open(msg mailIn) bool {
	mb := c.m.byID[msg.Mailbox]
	if mb == nil || !mb.claims[c.side] || c.box != nil {
		return c.fail("no such mailbox")
	}
	c.box = mb
	mb.listeners[c] = true
	for _, held := range mb.msgs {
		c.sendLocked(mailMap(held))
	}
	return true
}

func (c *mailConn) add(msg mailIn) bool {
	mb := c.box
	if mb == nil {
		return c.fail("open a mailbox first")
	}
	if len(mb.msgs) >= 16 {
		return c.fail("the mailbox is full")
	}
	held := mailMsg{Type: "message", Side: c.side, Phase: msg.Phase, Body: msg.Body,
		ID: strconv.Itoa(len(mb.msgs)), ServerRX: float64(c.m.s.now().UnixNano()) / 1e9}
	mb.msgs = append(mb.msgs, held)
	for l := range mb.listeners {
		l.sendLocked(mailMap(held))
	}
	return true
}

func (c *mailConn) release(msg mailIn) bool {
	if names := c.m.byName[c.app]; names != nil {
		if n, err := strconv.Atoi(msg.Nameplate); err == nil && names[n] != nil && names[n].claims[c.side] {
			delete(names, n)
		}
	}
	c.sendLocked(map[string]any{"type": "released"})
	return true
}

func (c *mailConn) closeBox(msg mailIn) bool {
	if mb := c.m.byID[msg.Mailbox]; mb != nil {
		mb.closed[c.side] = true
		delete(mb.listeners, c)
		// A failed exchange burns the code: whoever got it wrong gets no
		// second guess. The other side still hears the rest of the exchange,
		// so it fails at once rather than waiting out the code.
		if msg.Mood == "scary" {
			c.m.releaseLocked(mb)
		}
		if len(mb.closed) >= len(mb.claims) {
			c.m.deleteLocked(mb, c)
		}
	}
	c.sendLocked(map[string]any{"type": "closed"})
	return true
}

// sendLocked queues without blocking while m.mu is held: a listener too slow
// to take a pairing message is dropped rather than allowed to stall the rest.
func (c *mailConn) sendLocked(v map[string]any) {
	v["server_tx"] = float64(c.m.s.now().UnixNano()) / 1e9
	data, _ := json.Marshal(v)
	select {
	case c.out <- data:
	default:
		c.drop()
	}
}

func mailMap(msg mailMsg) map[string]any {
	return map[string]any{"type": msg.Type, "side": msg.Side, "phase": msg.Phase, "body": msg.Body,
		"id": msg.ID, "server_rx": msg.ServerRX}
}
