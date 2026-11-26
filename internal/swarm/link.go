package swarm

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/cyqlelabs/factor/internal/swarm/wire"
)

// window is how much one relayed stream may have in flight unread.
const window = 256 << 10

// errOffline is what a dial gets while the link is down.
var errOffline = errors.New("this agent is not connected to the rendezvous")

// Link is this agent's one outbound connection to its swarm's rendezvous. It
// carries presence, the roster and, when no direct path exists, the A2A
// traffic itself, multiplexed as streams the two ends encrypt end to end.
// It opens no listening port.
type Link struct {
	url   string
	id    *Identity
	swarm func() string
	hc    *http.Client

	onRoster func(*Roster)
	onAccept func(net.Conn, string)

	mu      sync.Mutex
	sess    *session
	state   string
	since   time.Time
	changed chan struct{}
}

// session is one live connection: a reconnect starts a new one, and every
// stream of the old one ends with it.
type session struct {
	l       *Link
	ws      *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	out     chan []byte
	outText chan []byte

	mu      sync.Mutex
	streams map[uint32]*stream
	nextSid uint32
	waiters map[string]chan Control
}

// NewLink makes a link to the rendezvous at url (wss://host). swarmID names
// the swarm this agent is in now, or "" while it is joining one.
func NewLink(url string, id *Identity, swarmID func() string) *Link {
	return &Link{url: strings.TrimRight(url, "/"), id: id, swarm: swarmID, hc: http.DefaultClient,
		state: "connecting", since: time.Now(), changed: make(chan struct{})}
}

// OnRoster is told every roster the rendezvous sends: pushed after a change,
// or the current one on connecting.
func (l *Link) OnRoster(fn func(*Roster)) { l.onRoster = fn }

// OnAccept is handed every stream another member opens to this agent, with
// the id the rendezvous says it came from. The TLS inside is what proves it.
func (l *Link) OnAccept(fn func(net.Conn, string)) { l.onAccept = fn }

// Status is the link's state in words, and since when.
func (l *Link) Status() (string, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state, l.since
}

// Online reports whether the link is connected.
func (l *Link) Online() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sess != nil
}

func (l *Link) setState(s *session, state string) {
	l.mu.Lock()
	l.sess, l.state, l.since = s, state, time.Now()
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

// WaitOnline blocks until the link is connected or ctx ends.
func (l *Link) WaitOnline(ctx context.Context) error {
	for {
		l.mu.Lock()
		on, ch := l.sess != nil, l.changed
		l.mu.Unlock()
		if on {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Run keeps the link up until ctx ends: it reconnects with a backoff after a
// failure, and at once after the machine wakes from sleep, when the old
// connection is dead whether or not anything has noticed yet.
func (l *Link) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := l.connect(ctx)
		if ctx.Err() != nil {
			break
		}
		l.setState(nil, "offline: "+err.Error())
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	l.setState(nil, "stopped")
}

func jitter(d time.Duration) time.Duration {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return d + time.Duration(binary.BigEndian.Uint16(b[:]))%(d/2+1)
}

// connect runs one session to its end.
func (l *Link) connect(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, _, err := websocket.Dial(dctx, l.url+"/v1/agent", &websocket.DialOptions{HTTPClient: l.hc})
	if err != nil {
		cancel()
		return err
	}
	ws.SetReadLimit(MaxFrame + 64<<10)
	var challenge Control
	if err := readCtl(dctx, ws, &challenge); err != nil || challenge.Type != CtlChallenge {
		cancel()
		_ = ws.CloseNow()
		return fmt.Errorf("the rendezvous sent no challenge: %v", err)
	}
	swarmID := l.swarm()
	hello := Control{Type: CtlHello, Key: EncodeKey(l.id.Public), Sig: l.id.SignHello(challenge.Nonce), Swarm: swarmID}
	if swarmID == "" {
		if hello.Work, err = wire.SolveWork(challenge.Nonce, hello.Key, challenge.Bits); err != nil {
			cancel()
			_ = ws.CloseNow()
			return err
		}
	}
	if err := writeCtl(dctx, ws, hello); err != nil {
		cancel()
		_ = ws.CloseNow()
		return err
	}
	var welcome Control
	if err := readCtl(dctx, ws, &welcome); err != nil {
		cancel()
		_ = ws.CloseNow()
		var ce websocket.CloseError
		if errors.As(err, &ce) && ce.Reason != "" {
			return errors.New(ce.Reason)
		}
		return err
	}
	cancel()
	if welcome.Type != CtlWelcome {
		_ = ws.CloseNow()
		return fmt.Errorf("the rendezvous refused this agent: %s", welcome.Error)
	}
	sctx, scancel := context.WithCancel(ctx)
	s := &session{l: l, ws: ws, ctx: sctx, cancel: scancel, out: make(chan []byte, 64),
		outText: make(chan []byte, 16), streams: map[uint32]*stream{}, waiters: map[string]chan Control{}}
	l.setState(s, "online")
	slog.Info("swarm link up", "rendezvous", l.url)
	if welcome.Roster != nil && l.onRoster != nil {
		l.onRoster(welcome.Roster)
	}
	go s.writeLoop()
	go s.watchSleep()
	go s.keepalive()
	err = s.readLoop()
	s.end(err)
	slog.Info("swarm link down", "rendezvous", l.url, "error", err)
	return err
}

// watchSleep ends the session when the machine has been asleep: the wall
// clock jumps while the monotonic one did not move, and a socket held across
// a suspend is usually dead without knowing it.
func (s *session) watchSleep() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	wall, mono := time.Now().Round(0), time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if time.Now().Round(0).Sub(wall)-time.Since(mono) > 10*time.Second {
				s.cancel()
				return
			}
			wall, mono = time.Now().Round(0), time.Now()
		}
	}
}

func (s *session) keepalive() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			err := s.ws.Ping(ctx)
			cancel()
			if err != nil {
				s.cancel()
				return
			}
		}
	}
}

func (s *session) writeLoop() {
	defer s.cancel()
	for {
		var typ websocket.MessageType
		var data []byte
		select {
		case <-s.ctx.Done():
			return
		case data = <-s.outText:
			typ = websocket.MessageText
		case data = <-s.out:
			typ = websocket.MessageBinary
		}
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		err := s.ws.Write(ctx, typ, data)
		cancel()
		if err != nil {
			return
		}
	}
}

func (s *session) readLoop() error {
	for {
		typ, data, err := s.ws.Read(s.ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) && ce.Reason != "" {
				return errors.New(ce.Reason)
			}
			return err
		}
		if typ == websocket.MessageText {
			var m Control
			if json.Unmarshal(data, &m) == nil {
				s.onControl(m)
			}
			continue
		}
		kind, sid, payload, err := wire.DecodeFrame(data)
		if err == nil {
			s.onFrame(kind, sid, payload)
		}
	}
}

// end fails every stream and request of a finished session.
func (s *session) end(err error) {
	s.cancel()
	_ = s.ws.CloseNow()
	if err == nil {
		err = errOffline
	}
	s.mu.Lock()
	streams, waiters := s.streams, s.waiters
	s.streams, s.waiters = map[uint32]*stream{}, map[string]chan Control{}
	s.mu.Unlock()
	for _, st := range streams {
		st.fail(fmt.Errorf("the connection to the rendezvous dropped: %w", err))
	}
	for _, w := range waiters {
		close(w)
	}
}

func (s *session) onControl(m Control) {
	s.mu.Lock()
	w, ok := s.waiters[m.Ref]
	if ok && m.Ref != "" {
		delete(s.waiters, m.Ref)
	}
	s.mu.Unlock()
	if ok && m.Ref != "" {
		w <- m
		return
	}
	if m.Type == CtlRoster && m.Roster != nil && s.l.onRoster != nil {
		s.l.onRoster(m.Roster)
	}
}

func (s *session) onFrame(kind byte, sid uint32, payload []byte) {
	if kind == FrameOpen {
		st := newStream(s, sid, string(payload))
		s.mu.Lock()
		s.streams[sid] = st
		s.mu.Unlock()
		close(st.opened)
		_ = s.frame(FrameCredit, sid, credit(window))
		if s.l.onAccept != nil {
			go s.l.onAccept(st, st.peer)
		} else {
			_ = st.Close()
		}
		return
	}
	s.mu.Lock()
	st := s.streams[sid]
	if kind == FrameClose {
		delete(s.streams, sid)
	}
	s.mu.Unlock()
	if st == nil {
		return
	}
	switch kind {
	case FrameData:
		st.push(payload)
	case FrameCredit:
		if len(payload) == 4 {
			st.grant(int(binary.BigEndian.Uint32(payload)))
		}
	case FrameClose:
		if len(payload) == 0 {
			st.fail(io.EOF)
		} else {
			st.fail(errors.New(string(payload)))
		}
	}
}

func credit(n int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(n))
	return b
}

// frame queues a relay frame on the session.
func (s *session) frame(kind byte, sid uint32, payload []byte) error {
	select {
	case s.out <- wire.EncodeFrame(kind, sid, payload):
		return nil
	case <-s.ctx.Done():
		return errOffline
	}
}

func (l *Link) current() (*session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess == nil {
		return nil, errOffline
	}
	return l.sess, nil
}

// Request sends a control message and waits for the reply that names it.
func (l *Link) Request(ctx context.Context, m Control) (Control, error) {
	s, err := l.current()
	if err != nil {
		return Control{}, err
	}
	ref := make([]byte, 8)
	_, _ = rand.Read(ref)
	m.Ref = hex.EncodeToString(ref)
	reply := make(chan Control, 1)
	s.mu.Lock()
	s.waiters[m.Ref] = reply
	s.mu.Unlock()
	data, _ := json.Marshal(m)
	select {
	case s.outText <- data:
	case <-ctx.Done():
		return Control{}, ctx.Err()
	case <-s.ctx.Done():
		return Control{}, errOffline
	}
	select {
	case r, ok := <-reply:
		if !ok {
			return Control{}, errOffline
		}
		if r.Type == CtlError {
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.waiters, m.Ref)
		s.mu.Unlock()
		return Control{}, ctx.Err()
	}
}

// Commit asks the rendezvous to make r the swarm's roster. It succeeds only
// when r follows the head the rendezvous holds.
func (l *Link) Commit(ctx context.Context, r *Roster) error {
	_, err := l.Request(ctx, Control{Type: CtlCommit, Roster: r})
	return err
}

// Fetch asks for the current roster of swarm.
func (l *Link) Fetch(ctx context.Context, swarmID string) (*Roster, error) {
	r, err := l.Request(ctx, Control{Type: CtlFetch, Swarm: swarmID})
	if err != nil {
		return nil, err
	}
	if r.Roster == nil {
		return nil, errors.New("the rendezvous sent no roster")
	}
	return r.Roster, nil
}

// Online members, as the rendezvous sees them.
func (l *Link) Presence(ctx context.Context) ([]string, error) {
	r, err := l.Request(ctx, Control{Type: CtlPresence})
	return r.Online, err
}

// Dial opens a relayed stream to peer.
func (l *Link) Dial(ctx context.Context, peer string) (net.Conn, error) {
	s, err := l.current()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.nextSid = (s.nextSid + 1) &^ (1 << 31)
	sid := s.nextSid
	st := newStream(s, sid, peer)
	s.streams[sid] = st
	s.mu.Unlock()
	if err := s.frame(FrameOpen, sid, []byte(peer)); err != nil {
		return nil, err
	}
	if err := s.frame(FrameCredit, sid, credit(window)); err != nil {
		return nil, err
	}
	select {
	case <-st.opened:
		return st, nil
	case <-st.dead:
		return nil, st.err()
	case <-ctx.Done():
		_ = st.Close()
		return nil, ctx.Err()
	}
}

// stream is one relayed byte stream, as a net.Conn. The sender never has
// more than the receiver's window in flight, so a slow reader slows its
// writer and nothing between them buffers without bound.
type stream struct {
	s    *session
	sid  uint32
	peer string

	opened   chan struct{}
	dead     chan struct{}
	deadOnce sync.Once
	rsig     chan struct{}
	wsig     chan struct{}

	mu       sync.Mutex
	rbuf     []byte
	rerr     error
	closed   bool
	credit   int
	unacked  int
	gotFirst bool
	rd, wd   time.Time
}

func newStream(s *session, sid uint32, peer string) *stream {
	return &stream{s: s, sid: sid, peer: peer, opened: make(chan struct{}), dead: make(chan struct{}),
		rsig: make(chan struct{}, 1), wsig: make(chan struct{}, 1)}
}

func notify(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (st *stream) push(p []byte) {
	st.mu.Lock()
	st.rbuf = append(st.rbuf, p...)
	st.mu.Unlock()
	notify(st.rsig)
}

func (st *stream) grant(n int) {
	st.mu.Lock()
	st.credit += n
	first := !st.gotFirst
	st.gotFirst = true
	st.mu.Unlock()
	if first {
		select {
		case <-st.opened:
		default:
			close(st.opened)
		}
	}
	notify(st.wsig)
}

func (st *stream) fail(err error) {
	st.mu.Lock()
	if st.rerr == nil {
		st.rerr = err
	}
	st.mu.Unlock()
	st.deadOnce.Do(func() { close(st.dead) })
	notify(st.rsig)
	notify(st.wsig)
}

func (st *stream) err() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.rerr == nil || errors.Is(st.rerr, io.EOF) {
		return errors.New("the other agent closed the stream")
	}
	return st.rerr
}

// wait blocks until sig fires or the deadline passes.
func wait(sig chan struct{}, deadline time.Time) bool {
	if deadline.IsZero() {
		<-sig
		return true
	}
	d := time.Until(deadline)
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-sig:
		return true
	case <-t.C:
		return false
	}
}

func (st *stream) Read(p []byte) (int, error) {
	for {
		st.mu.Lock()
		if len(st.rbuf) > 0 {
			n := copy(p, st.rbuf)
			st.rbuf = st.rbuf[n:]
			st.unacked += n
			give := 0
			if st.unacked >= window/2 {
				give, st.unacked = st.unacked, 0
			}
			st.mu.Unlock()
			if give > 0 {
				_ = st.s.frame(FrameCredit, st.sid, credit(give))
			}
			return n, nil
		}
		if st.closed {
			st.mu.Unlock()
			return 0, net.ErrClosed
		}
		if st.rerr != nil {
			err := st.rerr
			st.mu.Unlock()
			return 0, err
		}
		dl := st.rd
		st.mu.Unlock()
		if !wait(st.rsig, dl) {
			return 0, os.ErrDeadlineExceeded
		}
	}
}

func (st *stream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		st.mu.Lock()
		switch {
		case st.closed:
			st.mu.Unlock()
			return total, net.ErrClosed
		case st.rerr != nil:
			st.mu.Unlock()
			return total, st.err()
		case st.credit == 0:
			dl := st.wd
			st.mu.Unlock()
			if !wait(st.wsig, dl) {
				return total, os.ErrDeadlineExceeded
			}
			continue
		}
		n := min(len(p), st.credit, MaxFrame)
		st.credit -= n
		st.mu.Unlock()
		if err := st.s.frame(FrameData, st.sid, p[:n]); err != nil {
			return total, err
		}
		p, total = p[n:], total+n
	}
	return total, nil
}

func (st *stream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	alive := st.rerr == nil
	st.mu.Unlock()
	st.s.mu.Lock()
	delete(st.s.streams, st.sid)
	st.s.mu.Unlock()
	if alive {
		_ = st.s.frame(FrameClose, st.sid, nil)
	}
	st.deadOnce.Do(func() { close(st.dead) })
	notify(st.rsig)
	notify(st.wsig)
	return nil
}

type swarmAddr string

func (a swarmAddr) Network() string { return "swarm" }
func (a swarmAddr) String() string  { return string(a) }

func (st *stream) LocalAddr() net.Addr  { return swarmAddr(st.s.l.id.ID) }
func (st *stream) RemoteAddr() net.Addr { return swarmAddr(st.peer) }

func (st *stream) SetDeadline(t time.Time) error {
	_ = st.SetReadDeadline(t)
	return st.SetWriteDeadline(t)
}

func (st *stream) SetReadDeadline(t time.Time) error {
	st.mu.Lock()
	st.rd = t
	st.mu.Unlock()
	notify(st.rsig)
	return nil
}

func (st *stream) SetWriteDeadline(t time.Time) error {
	st.mu.Lock()
	st.wd = t
	st.mu.Unlock()
	notify(st.wsig)
	return nil
}

func writeCtl(ctx context.Context, ws *websocket.Conn, m Control) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, data)
}

func readCtl(ctx context.Context, ws *websocket.Conn, m *Control) error {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("expected a control message")
	}
	return json.Unmarshal(data, m)
}
