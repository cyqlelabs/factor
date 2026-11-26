package swarm

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// linked brings up a and b on one roster at an in-process rendezvous and
// returns their links.
func linked(t *testing.T) (r *relay, la, lb *Link, a, b *Identity) {
	t.Helper()
	r = newRelay(t)
	a, _ = NewIdentity()
	b, _ = NewIdentity()
	g := Genesis(a, NewSwarmID(), r.url)
	start := func(id *Identity, swarmID string) *Link {
		l := NewLink(r.url, id, func() string { return swarmID })
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go l.Run(ctx)
		wctx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		if err := l.WaitOnline(wctx); err != nil {
			t.Fatalf("link never came up: %v", err)
		}
		return l
	}
	la = start(a, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := la.Commit(ctx, g); err != nil {
		t.Fatalf("genesis: %v", err)
	}
	two := g.Next(a, func(x *Roster) { x.Add(EncodeKey(b.Public), RoleMember) })
	if err := la.Commit(ctx, two); err != nil {
		t.Fatalf("epoch 2: %v", err)
	}
	lb = start(b, g.Swarm)
	return r, la, lb, a, b
}

func TestLinkStreams(t *testing.T) {
	_, la, lb, _, b := linked(t)
	// More than a window, so the sender has to wait for credit to come back.
	payload := []byte(strings.Repeat("x", 3*window+123))
	got := make(chan []byte, 1)
	lb.OnAccept(func(c net.Conn, from string) {
		data := make([]byte, len(payload))
		_, _ = io.ReadFull(c, data)
		got <- data
		_, _ = c.Write([]byte("bye"))
		_ = c.Close()
	})
	conn, err := la.Dial(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-got:
		if len(data) != len(payload) {
			t.Fatalf("received %d of %d", len(data), len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never arrived")
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8)
	if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "bye" {
		t.Fatalf("read back %q %v", buf[:n], err)
	}
	if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
		t.Errorf("after the far side closed: %v", err)
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("wrote to a closed stream")
	}
	_ = conn.Close()
	if _, err := conn.Read(buf); !errors.Is(err, net.ErrClosed) {
		t.Errorf("read after close: %v", err)
	}
	if conn.RemoteAddr().String() != b.ID || conn.LocalAddr().Network() != "swarm" {
		t.Error("addresses")
	}
}

func TestLinkDeadlinesAndRefusals(t *testing.T) {
	r, la, lb, _, b := linked(t)
	lb.OnAccept(func(c net.Conn, _ string) {})
	conn, err := la.Dial(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read deadline: %v", err)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(-time.Second))
	conn.(*stream).mu.Lock()
	conn.(*stream).credit = 0
	conn.(*stream).mu.Unlock()
	if _, err := conn.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("write deadline: %v", err)
	}
	stranger, _ := NewIdentity()
	if _, err := la.Dial(context.Background(), stranger.ID); err == nil {
		t.Error("dialled an agent outside the swarm")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if online, err := la.Presence(ctx); err != nil || len(online) != 2 {
		t.Errorf("presence %v %v", online, err)
	}
	if head, err := lb.Fetch(ctx, ""); err != nil || head.Epoch != 2 {
		t.Errorf("fetch %v %v", head, err)
	}
	if _, err := la.Request(ctx, Control{Type: "dance"}); err == nil {
		t.Error("an unknown request was answered")
	}
	off := NewLink(r.url, stranger, func() string { return "" })
	if _, err := off.Dial(ctx, b.ID); !errors.Is(err, errOffline) {
		t.Errorf("dial while offline: %v", err)
	}
	if _, err := off.Request(ctx, Control{Type: CtlFetch}); !errors.Is(err, errOffline) {
		t.Errorf("request while offline: %v", err)
	}
	if state, _ := off.Status(); state != "connecting" || off.Online() {
		t.Errorf("status before running: %q", state)
	}
}

func TestAdoptRules(t *testing.T) {
	n := newNode(t, "desk")
	var told []string
	n.svc.OnNotice(func(s string) { told = append(told, s) })
	other, _ := NewIdentity()
	third, _ := NewIdentity()

	// A roster for a swarm this agent is not joining is refused.
	g := Genesis(other, NewSwarmID(), "wss://r")
	n.svc.adopt(g, true)
	if n.svc.book.Read().Joined() {
		t.Fatal("adopted a roster nobody asked for")
	}
	// Joining: the first roster that lists this agent is taken.
	two := g.Next(other, func(r *Roster) { r.Add(EncodeKey(n.svc.id.Public), RoleMember) })
	_ = n.svc.book.Update(func(st *State) error { st.Joining = g.Swarm; return nil })
	n.svc.adopt(two, true)
	st := n.svc.book.Read()
	if !st.Joined() || st.Joining != "" || st.Refreshed.IsZero() || st.Rendezvous != "wss://r" {
		t.Fatalf("join: %+v", st)
	}
	// The same roster again only refreshes it; an older one is a rollback.
	n.svc.adopt(two, true)
	n.svc.adopt(g, true)
	if n.svc.book.Read().Roster.Epoch != 2 {
		t.Error("rolled back")
	}
	// Another swarm's roster is ignored.
	n.svc.adopt(Genesis(third, NewSwarmID(), ""), true)
	// A roster that does not follow is refused.
	bad := two.Next(third, func(*Roster) {})
	n.svc.adopt(bad, true)
	if n.svc.book.Read().Roster.Epoch != 2 {
		t.Error("adopted a roster signed by a stranger")
	}
	// A jump of several epochs is taken on the signature.
	three := two.Next(other, func(r *Roster) { r.Add(EncodeKey(third.Public), RoleMember) })
	five := three.Next(other, func(*Roster) {}).Next(other, func(*Roster) {})
	n.svc.adopt(five, true)
	if n.svc.book.Read().Roster.Epoch != 5 || !n.svc.IsMember(third.ID) {
		t.Errorf("gap: epoch %d", n.svc.book.Read().Roster.Epoch)
	}
	// Two different rosters at one epoch freeze membership and say so.
	fork := three.Next(other, func(r *Roster) { r.Remove(EncodeKey(third.Public)) }).Next(other, func(*Roster) {})
	n.svc.adopt(fork, true)
	if n.svc.book.Read().Frozen == "" || len(told) == 0 || !strings.Contains(told[0], "frozen") {
		t.Errorf("fork: %q %v", n.svc.book.Read().Frozen, told)
	}
	if err := n.svc.commit(context.Background(), fork); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Errorf("commit while frozen: %v", err)
	}
	if !strings.Contains(n.svc.Status(), "offline") {
		t.Errorf("status %q", n.svc.Status())
	}
	_ = n.svc.book.Update(func(st *State) error { st.Frozen = ""; return nil })
	// Removing this agent tells the user.
	out := n.svc.book.Read().Roster.Next(other, func(r *Roster) { r.Remove(EncodeKey(n.svc.id.Public)) })
	n.svc.adopt(out, true)
	if !strings.Contains(told[len(told)-1], "removed") {
		t.Errorf("removal of this agent: %v", told)
	}
	n.svc.adopt(nil, true)
}

func TestMembershipErrors(t *testing.T) {
	n := newNode(t, "desk")
	ctx := context.Background()
	if err := n.svc.Found(ctx); err == nil {
		t.Error("founded a swarm with no link")
	}
	if err := n.svc.Admit(ctx, "x", "", ""); err == nil {
		t.Error("admitted outside a swarm")
	}
	if err := n.svc.commit(ctx, Genesis(n.svc.id, NewSwarmID(), "")); err == nil {
		t.Error("committed with no link")
	}
	home := newNode(t, "home")
	pair(t, map[string]*node{"desk": n, "home": home})
	if err := n.svc.Found(ctx); err != nil {
		t.Errorf("found while in a swarm: %v", err)
	}
	if err := n.svc.Admit(ctx, "not a key", "", ""); err == nil {
		t.Error("admitted a malformed key")
	}
	if err := n.svc.Admit(ctx, EncodeKey(home.svc.id.Public), "nas", ""); err != nil {
		t.Errorf("admitting a member again: %v", err)
	}
	if n.svc.book.Read().Label(home.svc.ID()) != "nas" {
		t.Error("admit did not rename")
	}
	if err := n.svc.Remove(ctx, n.svc.ID()); err == nil {
		t.Error("removed itself")
	}
	if err := n.svc.Remove(ctx, "nobody"); err == nil {
		t.Error("removed a stranger")
	}
	if res := n.svc.removeMember(ctx, "nobody"); !res.IsError {
		t.Error("remove tool with an unknown member")
	}
	if res := n.svc.removeMember(ctx, "nas"); !res.IsError {
		t.Error("removed with no link to commit through")
	}
	if n.svc.Status() == "" || n.svc.offlineText() != "offline" {
		t.Errorf("status %q", n.svc.Status())
	}
	n.svc.offline = "no rendezvous"
	if !strings.Contains(n.svc.Status(), "no rendezvous") {
		t.Errorf("status %q", n.svc.Status())
	}
}

func TestConnectWithoutRendezvous(t *testing.T) {
	n := newNode(t, "desk")
	n.svc.Connect()
	if !strings.Contains(n.svc.offline, "no rendezvous") {
		t.Errorf("offline %q", n.svc.offline)
	}
}

func TestNoticeTo(t *testing.T) {
	b := bus.New()
	NoticeTo(b, func() (string, string, bool) { return "telegram", "9", true })("hello")
	select {
	case m := <-b.Outbound():
		if m.Channel != "telegram" || m.ChatID != "9" || m.Content != "hello" {
			t.Errorf("%+v", m)
		}
	default:
		t.Error("nothing sent")
	}
	NoticeTo(b, func() (string, string, bool) { return "", "", false })("nowhere")
	if len(b.Outbound()) != 0 {
		t.Error("sent with no chat")
	}
}

func TestRemovalCancelsRunningTasks(t *testing.T) {
	r := newRelay(t)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 1)
	desk := r.agent("desk", func(agent.RemoteTurn) string {
		started <- struct{}{}
		<-release
		return "late"
	})
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
	// home hands desk something slow; desk removes home while it runs.
	done := make(chan *tools.Result, 1)
	go func() { done <- relaySend(t, home, desk.ID(), "ping") }()
	select {
	case <-started:
	case res := <-done:
		t.Fatalf("the task never started: %q", res.ForLLM)
	case <-time.After(10 * time.Second):
		t.Fatal("the task never started")
	}
	if err := desk.Remove(ctx, home.ID()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the removed member's task to end", func() bool { return desk.server.Running() == 0 })
	waitFor(t, "home to be cut off", func() bool { return !homeLink(home).Online() })
	<-done
}

func homeLink(s *Service) *Link {
	_, l, _ := s.net()
	return l
}
