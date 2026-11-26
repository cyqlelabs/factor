package rendezvous

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/cyqlelabs/factor/internal/swarm/wire"
)

// Config bounds what one rendezvous gives away. The defaults are placeholders
// for a free instance (open question 2 in docs/wire.md).
type Config struct {
	// Data is the store directory; Create makes one there if none exists.
	Data   string
	Create bool
	// RateBytes is what one agent may relay per second, sustained, BurstBytes
	// how far it may run ahead of that, and DailyBytes what it may relay in a
	// day.
	RateBytes  int
	BurstBytes int
	DailyBytes int64
	// WorkBits is the proof of work a key no roster lists owes before it is
	// registered, and a mailbox before it opens.
	WorkBits int
	// PerIPMinute is how many connections one address may open a minute.
	PerIPMinute int
	// TrustProxy reads the client's address from X-Forwarded-For, for a
	// rendezvous behind a TLS proxy that sets it.
	TrustProxy bool
	// MailboxTTL is how long a pairing mailbox lives.
	MailboxTTL time.Duration
}

// Defaults fills what cfg leaves unset.
func (c Config) withDefaults() Config {
	if c.RateBytes <= 0 {
		c.RateBytes = 32 << 10
	}
	if c.BurstBytes <= 0 {
		c.BurstBytes = 256 << 10
	}
	if c.DailyBytes <= 0 {
		c.DailyBytes = 15 << 20
	}
	if c.WorkBits < 0 {
		c.WorkBits = 0
	}
	if c.PerIPMinute <= 0 {
		c.PerIPMinute = 30
	}
	if c.MailboxTTL <= 0 {
		c.MailboxTTL = 10 * time.Minute
	}
	return c
}

// DefaultWorkBits is the proof of work asked by default: a fraction of a
// second on an old CPU, and a cost per key for anyone registering them by
// the thousand.
const DefaultWorkBits = 18

// Server is one rendezvous.
type Server struct {
	cfg   Config
	store *Store
	mail  *mailboxes
	now   func() time.Time

	mu    sync.Mutex
	conns map[string]*conn
	ips   map[string][]time.Time
	spent map[string]daySpend
}

type daySpend struct {
	day   string
	bytes int64
}

// New opens a rendezvous over the store in cfg.Data.
func New(cfg Config) (*Server, error) {
	cfg = cfg.withDefaults()
	store, err := OpenStore(cfg.Data, cfg.Create)
	if err != nil {
		return nil, err
	}
	if why := store.Refusing(); why != "" {
		slog.Warn("the rendezvous refuses roster changes", "reason", why)
	}
	s := &Server{cfg: cfg, store: store, now: time.Now,
		conns: map[string]*conn{}, ips: map[string][]time.Time{}, spent: map[string]daySpend{}}
	s.mail = newMailboxes(s)
	return s, nil
}

// Handler serves the agent socket, the pairing mailboxes and a health check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent", s.serveAgent)
	mux.HandleFunc("GET /v1/mailbox", s.mail.serve)
	mux.HandleFunc("GET /v1/work", s.mail.serveWork)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		online := len(s.conns)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": s.store.Refusing() == "",
			"online": online, "swarms": s.store.Swarms(), "refusing": s.store.Refusing()})
	})
	return mux
}

// admitIP applies the per-address connection limit.
func (s *Server) admitIP(r *http.Request) bool {
	ip := s.clientIP(r)
	cutoff := s.now().Add(-time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	hits := s.ips[ip][:0]
	for _, t := range s.ips[ip] {
		if t.After(cutoff) {
			hits = append(hits, t)
		}
	}
	if len(hits) >= s.cfg.PerIPMinute {
		s.ips[ip] = hits
		return false
	}
	s.ips[ip] = append(hits, s.now())
	if len(s.ips) > 10000 {
		// A map keyed by address only ever grows; a minute is all it needs.
		for k, v := range s.ips {
			if len(v) == 0 || v[len(v)-1].Before(cutoff) {
				delete(s.ips, k)
			}
		}
	}
	return true
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newNonce() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// conn is one agent's socket.
type conn struct {
	s   *Server
	ws  *websocket.Conn
	id  string
	key string
	out chan outbound
	ctx context.Context

	// Guarded by Server.mu.
	swarm  string
	routes map[uint32]route
	nextIn uint32

	bucket bucket
}

type route struct {
	peer *conn
	sid  uint32
}

type outbound struct {
	typ  websocket.MessageType
	data []byte
}

// inbound stream ids, which the rendezvous assigns, have the top bit set;
// the ones an agent picks for the streams it opens do not.
const inboundBit = 1 << 31

func (s *Server) serveAgent(w http.ResponseWriter, r *http.Request) {
	if !s.admitIP(r) {
		http.Error(w, "too many connections from this address", http.StatusTooManyRequests)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(wire.MaxFrame + 64<<10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &conn{s: s, ws: ws, out: make(chan outbound, 256), ctx: ctx, routes: map[uint32]route{},
		bucket: bucket{rate: float64(s.cfg.RateBytes), burst: float64(s.cfg.BurstBytes),
			tokens: float64(s.cfg.BurstBytes), last: s.now()}}
	if err := c.handshake(ctx); err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	go c.writeLoop(ctx, cancel)
	defer s.drop(c)
	go c.keepalive(ctx, cancel)
	// A key no roster lists is here to pair, which takes a code's lifetime;
	// past that, it is holding a socket for nothing.
	timer := time.AfterFunc(s.cfg.MailboxTTL, func() {
		s.mu.Lock()
		unlisted := c.swarm == ""
		s.mu.Unlock()
		if unlisted {
			_ = ws.Close(websocket.StatusPolicyViolation, "a key no roster lists has only a code's lifetime to pair")
		}
	})
	defer timer.Stop()
	c.readLoop(ctx)
}

// handshake authenticates the agent: it signs a fresh nonce with its key, and
// a key no roster lists also pays the proof of work.
func (c *conn) handshake(ctx context.Context) error {
	s := c.s
	nonce := newNonce()
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := writeJSON(hctx, c.ws, wire.Control{Type: wire.CtlChallenge, Nonce: nonce, Bits: s.cfg.WorkBits}); err != nil {
		return err
	}
	var hello wire.Control
	if err := readJSON(hctx, c.ws, &hello); err != nil {
		return err
	}
	if hello.Type != wire.CtlHello {
		return errors.New("expected hello")
	}
	pub, err := wire.VerifyHello(hello.Key, nonce, hello.Sig)
	if err != nil {
		return err
	}
	c.id, c.key = wire.IDOf(pub), hello.Key
	var head *wire.Roster
	if hello.Swarm != "" {
		head = s.store.Head(hello.Swarm)
	}
	switch {
	case head.HasKey(c.key):
		c.swarm = hello.Swarm
	case head.WasRemoved(c.id):
		return errors.New("this agent was removed from the swarm")
	case !wire.CheckWork(nonce, c.key, hello.Work, s.cfg.WorkBits):
		return errors.New("a new key owes the proof of work")
	default:
		head = nil
	}
	s.mu.Lock()
	if old := s.conns[c.id]; old != nil {
		_ = old.ws.Close(websocket.StatusGoingAway, "replaced by a newer connection")
	}
	s.conns[c.id] = c
	s.mu.Unlock()
	return writeJSON(hctx, c.ws, wire.Control{Type: wire.CtlWelcome, ID: c.id, Roster: head})
}

func (c *conn) keepalive(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, done := context.WithTimeout(ctx, 10*time.Second)
			err := c.ws.Ping(pctx)
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func (c *conn) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.out:
			wctx, done := context.WithTimeout(ctx, 30*time.Second)
			err := c.ws.Write(wctx, m.typ, m.data)
			done()
			if err != nil {
				return
			}
		}
	}
}

// send queues a message, waiting while the queue is full: a slow reader
// slows its senders down rather than growing a buffer here.
func (c *conn) send(typ websocket.MessageType, data []byte) {
	select {
	case c.out <- outbound{typ: typ, data: data}:
	case <-c.ctx.Done():
	}
}

func (c *conn) control(m wire.Control) {
	data, _ := json.Marshal(m)
	c.send(websocket.MessageText, data)
}

func (c *conn) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var m wire.Control
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			c.onControl(m)
			continue
		}
		kind, sid, payload, err := wire.DecodeFrame(data)
		if err != nil {
			continue
		}
		c.onFrame(kind, sid, payload)
	}
}

func (c *conn) onControl(m wire.Control) {
	s := c.s
	switch m.Type {
	case wire.CtlCommit:
		if m.Roster == nil || m.Roster.Signer != c.key {
			c.control(wire.Control{Type: wire.CtlError, Ref: m.Ref, Error: "a roster is committed by the member who signed it"})
			return
		}
		if err := s.store.Commit(m.Roster); err != nil {
			c.control(wire.Control{Type: wire.CtlError, Ref: m.Ref, Error: err.Error()})
			return
		}
		c.control(wire.Control{Type: wire.CtlCommitted, Ref: m.Ref, Swarm: m.Roster.Swarm})
		s.announce(m.Roster)
	case wire.CtlFetch:
		s.mu.Lock()
		id := c.swarm
		s.mu.Unlock()
		if m.Swarm != "" && id == "" {
			// A newcomer whose roster was committed while it waited.
			if head := s.store.Head(m.Swarm); head.HasKey(c.key) {
				id = m.Swarm
				s.mu.Lock()
				c.swarm = id
				s.mu.Unlock()
			}
		}
		head := s.store.Head(id)
		if head == nil {
			c.control(wire.Control{Type: wire.CtlError, Ref: m.Ref, Error: "this agent is not on any roster here"})
			return
		}
		c.control(wire.Control{Type: wire.CtlRoster, Ref: m.Ref, Roster: head})
	case wire.CtlPresence:
		c.control(wire.Control{Type: wire.CtlPresence, Ref: m.Ref, Online: s.online(c)})
	default:
		c.control(wire.Control{Type: wire.CtlError, Ref: m.Ref, Error: "unknown request " + m.Type})
	}
}

// announce pushes a newly committed roster to every member online, and
// cuts off the keys it removed.
func (s *Server) announce(r *wire.Roster) {
	data, _ := json.Marshal(wire.Control{Type: wire.CtlRoster, Roster: r})
	s.mu.Lock()
	var push, cut []*conn
	for _, c := range s.conns {
		switch {
		case r.HasKey(c.key):
			c.swarm = r.Swarm
			push = append(push, c)
		case c.swarm == r.Swarm:
			c.swarm = ""
			cut = append(cut, c)
		}
	}
	s.mu.Unlock()
	for _, c := range push {
		c.send(websocket.MessageText, data)
	}
	for _, c := range cut {
		_ = c.ws.Close(websocket.StatusPolicyViolation, "this agent was removed from the swarm")
	}
}

func (s *Server) online(c *conn) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if c.swarm == "" {
		return out
	}
	for id, other := range s.conns {
		if other.swarm == c.swarm {
			out = append(out, id)
		}
	}
	return out
}

func (c *conn) onFrame(kind byte, sid uint32, payload []byte) {
	s := c.s
	switch kind {
	case wire.FrameOpen:
		c.open(sid, string(payload))
		return
	case wire.FrameData:
		if why := s.charge(c, len(payload)); why != "" {
			c.closeStream(sid, why)
			return
		}
	}
	s.mu.Lock()
	r, ok := c.routes[sid]
	if kind == wire.FrameClose {
		delete(c.routes, sid)
		if ok {
			delete(r.peer.routes, r.sid)
		}
	}
	s.mu.Unlock()
	if !ok {
		if kind != wire.FrameClose {
			c.send(websocket.MessageBinary, wire.EncodeFrame(wire.FrameClose, sid, []byte("no such stream")))
		}
		return
	}
	r.peer.send(websocket.MessageBinary, wire.EncodeFrame(kind, r.sid, payload))
}

// open routes a new stream to another member of the same roster, and to
// nobody else: the relay is never an open proxy.
func (c *conn) open(sid uint32, target string) {
	s := c.s
	refuse := func(why string) {
		c.send(websocket.MessageBinary, wire.EncodeFrame(wire.FrameClose, sid, []byte(why)))
	}
	if sid&inboundBit != 0 {
		refuse("stream ids with the top bit set are the rendezvous's to assign")
		return
	}
	s.mu.Lock()
	peer := s.conns[target]
	switch {
	case c.swarm == "":
		s.mu.Unlock()
		refuse("this agent is not on a roster")
		return
	case peer == nil || peer.swarm != c.swarm || peer == c:
		s.mu.Unlock()
		refuse("that agent is not online in this swarm")
		return
	}
	if _, busy := c.routes[sid]; busy {
		s.mu.Unlock()
		refuse("that stream id is in use")
		return
	}
	peer.nextIn = (peer.nextIn + 1) &^ inboundBit
	psid := peer.nextIn | inboundBit
	c.routes[sid] = route{peer: peer, sid: psid}
	peer.routes[psid] = route{peer: c, sid: sid}
	s.mu.Unlock()
	peer.send(websocket.MessageBinary, wire.EncodeFrame(wire.FrameOpen, psid, []byte(c.id)))
}

func (c *conn) closeStream(sid uint32, why string) {
	s := c.s
	s.mu.Lock()
	r, ok := c.routes[sid]
	delete(c.routes, sid)
	if ok {
		delete(r.peer.routes, r.sid)
	}
	s.mu.Unlock()
	c.send(websocket.MessageBinary, wire.EncodeFrame(wire.FrameClose, sid, []byte(why)))
	if ok {
		r.peer.send(websocket.MessageBinary, wire.EncodeFrame(wire.FrameClose, r.sid, []byte(why)))
	}
}

// charge counts relayed bytes against the agent's quotas. It waits while the
// sustained rate is exceeded, which slows the sender down; past the daily
// quota it refuses.
func (s *Server) charge(c *conn, n int) string {
	day := s.now().Format("2006-01-02")
	s.mu.Lock()
	spent := s.spent[c.id]
	if spent.day != day {
		spent = daySpend{day: day}
	}
	if spent.bytes+int64(n) > s.cfg.DailyBytes {
		s.mu.Unlock()
		return "this agent has relayed its daily quota"
	}
	spent.bytes += int64(n)
	s.spent[c.id] = spent
	s.mu.Unlock()
	if wait := c.bucket.take(float64(n), s.now()); wait > 0 {
		select {
		case <-time.After(wait):
		case <-c.ctx.Done():
		}
	}
	return ""
}

// drop forgets a connection and closes every stream it carried.
func (s *Server) drop(c *conn) {
	s.mu.Lock()
	if s.conns[c.id] == c {
		delete(s.conns, c.id)
	}
	routes := c.routes
	c.routes = map[uint32]route{}
	for _, r := range routes {
		delete(r.peer.routes, r.sid)
	}
	s.mu.Unlock()
	for _, r := range routes {
		r.peer.send(websocket.MessageBinary, wire.EncodeFrame(wire.FrameClose, r.sid, []byte("the other agent went offline")))
	}
	_ = c.ws.CloseNow()
}

// bucket is a token bucket: rate bytes a second, up to burst saved up.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// take spends n tokens and says how long to wait before the bytes go on.
func (b *bucket) take(n float64, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	b.tokens -= n
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

func writeJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, data)
}

func readJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return fmt.Errorf("expected a text message")
	}
	return json.Unmarshal(data, v)
}
