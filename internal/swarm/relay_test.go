package swarm

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/rendezvous"
	"github.com/cyqlelabs/factor/internal/tools"
)

type replyTurns struct {
	reply    func(agent.RemoteTurn) string
	audience string
}

// ProcessRemote honours cancellation the way the loop does.
func (t replyTurns) ProcessRemote(ctx context.Context, rt agent.RemoteTurn) (string, error) {
	done := make(chan string, 1)
	go func() { done <- t.reply(rt) }()
	select {
	case r := <-done:
		return r, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (t replyTurns) AudienceAt(string, string) string { return t.audience }

// relay is an in-process rendezvous, reached over a real socket.
type relay struct {
	t   *testing.T
	srv *rendezvous.Server
	hs  *httptest.Server
	url string
	// refreshEvery is the roster refresh its agents get.
	refreshEvery time.Duration
}

func newRelay(t *testing.T) *relay { return newSlowRelay(t, 0) }

// newSlowRelay holds back everything the rendezvous sends by d, so a round
// trip between two agents through it takes at least 2d.
func newSlowRelay(t *testing.T, d time.Duration) *relay {
	t.Helper()
	srv, err := rendezvous.New(rendezvous.Config{Data: filepath.Join(t.TempDir(), "store"), Create: true, WorkBits: 4,
		RateBytes: 1 << 30, BurstBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewUnstartedServer(srv.Handler())
	if d > 0 {
		hs.Listener = slowListener{hs.Listener, d}
	}
	hs.Start()
	t.Cleanup(hs.Close)
	return &relay{t: t, srv: srv, hs: hs, url: "ws" + strings.TrimPrefix(hs.URL, "http"), refreshEvery: refreshEvery}
}

func (r *relay) agent(name string, reply func(agent.RemoteTurn) string) *Service {
	r.t.Helper()
	svc, err := New(config.SwarmConfig{Enabled: true, Name: name, Rendezvous: r.url, RemoteTasks: 2, RemoteQueue: 2},
		r.t.TempDir(), "test", replyTurns{reply: reply}, bus.New())
	if err != nil {
		r.t.Fatal(err)
	}
	svc.refreshEvery = r.refreshEvery
	svc.Connect()
	r.t.Cleanup(svc.Close)
	return svc
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func relaySend(t *testing.T, from *Service, to, message string) *tools.Result {
	t.Helper()
	tc := tools.ToolContext{Channel: "telegram", ChatID: "1", SessionKey: "telegram:1", Trigger: "user"}
	return from.Tools()[0].Execute(tools.WithToolContext(context.Background(), tc),
		map[string]any{"agent": to, "message": message})
}

func TestTwoAgentsThroughTheRelay(t *testing.T) {
	r := newRelay(t)
	desk := r.agent("desk", func(agent.RemoteTurn) string { return "desk here" })
	home := r.agent("home box", func(rt agent.RemoteTurn) string { return "home saw: " + rt.Content })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := desk.Found(ctx); err != nil {
		t.Fatalf("found: %v", err)
	}
	// The newcomer waits to see itself on the roster of the swarm it paired
	// into; pairing is what tells it which one.
	swarmID := desk.Book().Read().Roster.Swarm
	if err := home.Book().Update(func(st *State) error { st.Joining = swarmID; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := desk.Admit(ctx, EncodeKey(home.Identity().Public), "home box", ""); err != nil {
		t.Fatalf("admit: %v", err)
	}
	waitFor(t, "the newcomer to adopt the roster", func() bool { return home.Book().Read().Joined() })
	if desk.Book().Read().Roster.Epoch != 2 || home.Book().Read().Roster.Hash() != desk.Book().Read().Roster.Hash() {
		t.Fatal("the two agents hold different rosters")
	}
	// Each learns the other's name over the tunnel.
	waitFor(t, "names", func() bool { return home.Book().Read().Peer(desk.ID()).Name == "desk" })

	start := time.Now()
	res := relaySend(t, desk, "home box", "ping")
	first := time.Since(start)
	if res.IsError || !strings.Contains(res.ForLLM, "home saw: ping") {
		t.Fatalf("first call: %q", res.ForLLM)
	}
	start = time.Now()
	res = relaySend(t, desk, "home box", "again")
	warm := time.Since(start)
	if res.IsError || !strings.Contains(res.ForLLM, "home saw: again") {
		t.Fatalf("warm call: %q", res.ForLLM)
	}
	t.Logf("relayed, in process: first call %s, warm call %s", first, warm)
	if !strings.Contains(home.Status(), "online") || !strings.Contains(home.Status(), "2 members") {
		t.Errorf("status %q", home.Status())
	}

	// The other way round works too.
	if res := relaySend(t, home, "desk", "hello"); !strings.Contains(res.ForLLM, "desk here") {
		t.Fatalf("home to desk: %q", res.ForLLM)
	}

	// Removing a member reaches every member and the rendezvous at once.
	if err := desk.Remove(ctx, home.ID()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if desk.IsMember(home.ID()) {
		t.Error("the remover still counts the removed agent")
	}
	if res := relaySend(t, desk, home.ID(), "still there?"); !res.IsError {
		t.Errorf("sent to a removed agent: %q", res.ForLLM)
	}
	waitFor(t, "the removed agent to be cut off", func() bool { return !strings.Contains(home.Status(), "online") })
	waitFor(t, "the removed agent to see the roster that removed it", func() bool { return !home.Book().Read().Joined() })
	// It cannot come back.
	if err := desk.Admit(ctx, EncodeKey(home.Identity().Public), "", ""); err == nil {
		t.Error("a removed agent was admitted again")
	}
}

// An agent removed while it was away learns it from the roster the
// rendezvous refuses it with, instead of keeping a stale one for good.
func TestRemovedWhileAway(t *testing.T) {
	r := newRelay(t)
	desk := r.agent("desk", func(agent.RemoteTurn) string { return "" })
	home := r.agent("home", func(agent.RemoteTurn) string { return "" })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := desk.Found(ctx); err != nil {
		t.Fatal(err)
	}
	_ = home.Book().Update(func(st *State) error { st.Joining = desk.Book().Read().Roster.Swarm; return nil })
	if err := desk.Admit(ctx, EncodeKey(home.Identity().Public), "home", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "join", func() bool { return home.Book().Read().Joined() })
	home.disconnect()
	waitFor(t, "the rendezvous to see home leave", func() bool { return r.online() == 1 })
	if err := desk.Remove(ctx, home.ID()); err != nil {
		t.Fatal(err)
	}
	// A change after the removal, which home is not shown.
	later, _ := NewIdentity()
	if err := desk.Admit(ctx, EncodeKey(later.Public), "later", ""); err != nil {
		t.Fatal(err)
	}
	home.Connect()
	waitFor(t, "home to learn it was removed", func() bool { return !home.Book().Read().Joined() })
}

// A warm call through the relay costs one round trip. The worker's pushes
// used to go out before each event reached the caller's stream, one round
// trip apiece, which made it five.
func TestWarmRelayedCallIsOneRoundTrip(t *testing.T) {
	const rtt = 40 * time.Millisecond
	old := writtenWait
	writtenWait = 15 * time.Second
	t.Cleanup(func() { writtenWait = old })
	r := newSlowRelay(t, rtt/2)
	desk := r.agent("desk", func(agent.RemoteTurn) string { return "" })
	home := r.agent("home", func(rt agent.RemoteTurn) string { return "home saw: " + rt.Content })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := desk.Found(ctx); err != nil {
		t.Fatal(err)
	}
	_ = home.Book().Update(func(st *State) error { st.Joining = desk.Book().Read().Roster.Swarm; return nil })
	if err := desk.Admit(ctx, EncodeKey(home.Identity().Public), "home", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "join", func() bool { return home.Book().Read().Joined() })
	var warm []time.Duration
	for i := range 8 {
		start := time.Now()
		res := relaySend(t, desk, home.ID(), "ping")
		took := time.Since(start)
		if res.IsError || !strings.Contains(res.ForLLM, "home saw: ping") {
			t.Fatalf("call %d: %q", i, res.ForLLM)
		}
		if i > 0 {
			warm = append(warm, took)
		}
	}
	slices.Sort(warm)
	if median := warm[len(warm)/2]; median > 3*rtt {
		t.Errorf("a warm relayed call took %s, more than three %s round trips", median, rtt)
	}
}

// slowListener hands out connections whose writes arrive d late.
type slowListener struct {
	net.Listener
	d time.Duration
}

func (l slowListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := &slowConn{Conn: c, q: make(chan slowWrite, 1024), done: make(chan struct{})}
	go sc.pump(l.d)
	return sc, nil
}

type slowWrite struct {
	b  []byte
	at time.Time
}

// slowConn delays its writes without holding up the writer, the way a long
// link does.
type slowConn struct {
	net.Conn
	q    chan slowWrite
	done chan struct{}
	once sync.Once
}

func (c *slowConn) Write(b []byte) (int, error) {
	select {
	case c.q <- slowWrite{append([]byte(nil), b...), time.Now()}:
		return len(b), nil
	case <-c.done:
		return 0, net.ErrClosed
	}
}

func (c *slowConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

func (c *slowConn) pump(d time.Duration) {
	for {
		select {
		case w := <-c.q:
			time.Sleep(time.Until(w.at.Add(d)))
			if _, err := c.Conn.Write(w.b); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (r *relay) online() int {
	resp, err := http.Get(r.hs.URL + "/healthz")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var h struct {
		Online int `json:"online"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h.Online
}
