package swarm

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tunnel is how this agent and the others reach each other: mutual TLS 1.3
// with each side's key pinned, and HTTP/2 inside, over whatever path there
// is — a stream relayed through the rendezvous, or a direct connection when
// one exists. A2A rides it as https://<agent-id>.swarm/.
//
// Both halves use the HTTP/2 the standard library already carries: the
// client negotiates it over the TLS connections it dials, and the server
// takes it from connections handed to it already wrapped in TLS.
type Tunnel struct {
	id     *Identity
	link   *Link
	member func(string) bool
	client *http.Client
	server *http.Server
	ln     *chanListener

	// direct dials a member without the relay, when it can; nil or an error
	// means the relay.
	direct func(ctx context.Context, peer string) (net.Conn, error)

	mu      sync.Mutex
	inbound map[string]map[net.Conn]bool
}

// keepalive is how often an idle tunnel is pinged: often enough to keep a
// recently used path warm and to find a dead one before a request does.
const keepalive = 15 * time.Second

// NewTunnel serves handler to the members link brings in, and reaches them
// through it.
func NewTunnel(id *Identity, link *Link, member func(string) bool, handler http.Handler) *Tunnel {
	t := &Tunnel{id: id, link: link, member: member, ln: newChanListener(id.ID),
		inbound: map[string]map[net.Conn]bool{}}
	h2 := &http.HTTP2Config{SendPingTimeout: keepalive, PingTimeout: 10 * time.Second}
	t.client = &http.Client{Transport: &http.Transport{
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return t.dialTLS(ctx, addr)
		},
		ForceAttemptHTTP2: true,
		HTTP2:             h2,
		IdleConnTimeout:   10 * time.Minute,
	}}
	t.server = &http.Server{Handler: handler, HTTP2: h2, ReadHeaderTimeout: 30 * time.Second,
		ConnState: t.track}
	if link != nil {
		link.OnAccept(func(conn net.Conn, _ string) { t.Accept(conn) })
	}
	go func() { _ = t.server.Serve(t.ln) }()
	return t
}

// SetDirect installs a way to reach members without the relay.
func (t *Tunnel) SetDirect(dial func(ctx context.Context, peer string) (net.Conn, error)) {
	t.direct = dial
}

// Close stops serving.
func (t *Tunnel) Close() {
	_ = t.server.Close()
	if tr, ok := t.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// HTTPClient implements Transport.
func (t *Tunnel) HTTPClient(peer string) (*http.Client, string, error) {
	if !t.member(peer) {
		return nil, "", fmt.Errorf("agent %s is not a member of this swarm", peer)
	}
	return t.client, "https://" + peer + HostSuffix + "/", nil
}

func (t *Tunnel) dialTLS(ctx context.Context, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	peer, ok := strings.CutSuffix(host, HostSuffix)
	if !ok || !t.member(peer) {
		return nil, fmt.Errorf("%s is not a member of this swarm", host)
	}
	var raw net.Conn
	if t.direct != nil {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		raw, err = t.direct(dctx, peer)
		cancel()
		if err != nil {
			raw = nil
		}
	}
	if raw == nil {
		if t.link == nil {
			return nil, errOffline
		}
		if raw, err = t.link.Dial(ctx, peer); err != nil {
			return nil, err
		}
	}
	conn := tls.Client(raw, t.id.ClientTLS(peer))
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// Accept serves one inbound connection from any path: pinned TLS first, then
// HTTP/2 until it ends. A key the roster does not list fails the handshake.
func (t *Tunnel) Accept(raw net.Conn) { t.ln.push(tls.Server(raw, t.id.ServerTLS(t.member))) }

// track keeps the open connections by the member they came from, so a
// removal can close them.
func (t *Tunnel) track(c net.Conn, state http.ConnState) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return
	}
	switch state {
	case http.StateActive:
		peer, ok := PeerID(tc.ConnectionState())
		if !ok {
			return
		}
		t.mu.Lock()
		if t.inbound[peer] == nil {
			t.inbound[peer] = map[net.Conn]bool{}
		}
		t.inbound[peer][c] = true
		t.mu.Unlock()
	case http.StateClosed, http.StateHijacked:
		t.mu.Lock()
		for _, conns := range t.inbound {
			delete(conns, c)
		}
		t.mu.Unlock()
	}
}

// Drop closes every connection from peer, which a removal calls for: a
// member taken off the roster loses what it already had open.
func (t *Tunnel) Drop(peer string) {
	t.mu.Lock()
	conns := t.inbound[peer]
	delete(t.inbound, peer)
	t.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
	if tr, ok := t.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// chanListener is a net.Listener fed by hand: relayed streams and direct
// connections alike, each wrapped in the server's pinned TLS.
type chanListener struct {
	addr  swarmAddr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newChanListener(self string) *chanListener {
	return &chanListener{addr: swarmAddr(self), conns: make(chan net.Conn, 16), done: make(chan struct{})}
}

func (l *chanListener) push(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	case <-time.After(10 * time.Second):
		slog.Warn("swarm: an inbound connection waited too long to be served")
		_ = c.Close()
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
