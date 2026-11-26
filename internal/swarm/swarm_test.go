package swarm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fa2a "github.com/cyqlelabs/factor/internal/a2a"
	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/cost"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

func init() {
	spokenWait = 300 * time.Millisecond
	writtenWait = 300 * time.Millisecond
	pollEvery = 50 * time.Millisecond
	attachWait = 500 * time.Millisecond
}

// fakeTurns stands in for the agent loop.
type fakeTurns struct {
	mu       sync.Mutex
	run      func(ctx context.Context, rt agent.RemoteTurn) (string, error)
	got      []agent.RemoteTurn
	audience string
}

func (f *fakeTurns) ProcessRemote(ctx context.Context, rt agent.RemoteTurn) (string, error) {
	f.mu.Lock()
	f.got = append(f.got, rt)
	run := f.run
	f.mu.Unlock()
	return run(ctx, rt)
}

func (f *fakeTurns) AudienceAt(string, string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audience
}

func (f *fakeTurns) setAudience(a string) {
	f.mu.Lock()
	f.audience = a
	f.mu.Unlock()
}

func (f *fakeTurns) turns() []agent.RemoteTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// loopback reaches the other test agents over plain HTTP, the caller named
// by a header the server trusts in place of a pinned certificate.
type loopback struct {
	self  string
	peers map[string]string
}

func (l *loopback) HTTPClient(peer string) (*http.Client, string, error) {
	url, ok := l.peers[peer]
	if !ok {
		return nil, "", errors.New("no route to " + peer)
	}
	return &http.Client{Transport: headerTransport{self: l.self}}, url, nil
}

type headerTransport struct{ self string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Peer", h.self)
	return http.DefaultTransport.RoundTrip(r)
}

type node struct {
	svc   *Service
	turns *fakeTurns
	bus   *bus.MessageBus
	http  *httptest.Server
	dir   string
}

func newNode(t *testing.T, name string) *node {
	t.Helper()
	dir := t.TempDir()
	turns := &fakeTurns{run: func(context.Context, agent.RemoteTurn) (string, error) { return "ok", nil }}
	b := bus.New()
	svc, err := New(config.SwarmConfig{Enabled: true, Name: name, RemoteTasks: 1, RemoteQueue: 1}, dir, "test", turns, b)
	if err != nil {
		t.Fatal(err)
	}
	n := &node{svc: svc, turns: turns, bus: b, dir: dir}
	n.serve(t)
	return n
}

func (n *node) serve(t *testing.T) {
	n.svc.peerOf = func(r *http.Request) (string, bool) {
		id := r.Header.Get("X-Test-Peer")
		return id, n.svc.IsMember(id)
	}
	n.http = httptest.NewServer(n.svc.Handler())
	t.Cleanup(func() {
		n.http.Close()
		n.svc.Close()
	})
}

// pair puts nodes on one roster, each calling the others by name.
func pair(t *testing.T, nodes map[string]*node) {
	t.Helper()
	var first *node
	for _, n := range nodes {
		first = n
		break
	}
	roster := Genesis(first.svc.id, NewSwarmID(), "wss://rendezvous.test")
	for _, n := range nodes {
		if n != first {
			roster = roster.Next(first.svc.id, func(r *Roster) { r.Add(EncodeKey(n.svc.id.Public), RoleMember) })
		}
	}
	for _, n := range nodes {
		lb := &loopback{self: n.svc.id.ID, peers: map[string]string{}}
		err := n.svc.book.Update(func(st *State) error {
			st.Roster = roster
			st.Refreshed = time.Now()
			for name, other := range nodes {
				if other != n {
					st.peer(other.svc.id.ID).Name = name
					lb.peers[other.svc.id.ID] = other.http.URL
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		n.svc.SetTransport(lb)
	}
}

func call(t *testing.T, tool tools.Tool, tc tools.ToolContext, args map[string]any) *tools.Result {
	t.Helper()
	ctx := tools.WithToolContext(context.Background(), tc)
	return tool.Execute(ctx, args)
}

func sendTo(n *node) tools.Tool  { return n.svc.Tools()[0] }
func adminOf(n *node) tools.Tool { return n.svc.Tools()[1] }

var telegram = tools.ToolContext{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42", Trigger: "user"}

func inbound(t *testing.T, b *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	select {
	case msg := <-b.Inbound():
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was delivered")
	}
	return bus.InboundMessage{}
}

func noInbound(t *testing.T, b *bus.MessageBus) {
	t.Helper()
	select {
	case msg := <-b.Inbound():
		t.Fatalf("unexpected delivery: %q", msg.Content)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDelegateQuickAnswer(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	home.turns.run = func(context.Context, agent.RemoteTurn) (string, error) { return "40 GB free", nil }

	res := call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "how much disk?"})
	if res.IsError || !strings.Contains(res.ForLLM, "40 GB free") {
		t.Fatalf("result %q", res.ForLLM)
	}
	got := home.turns.turns()
	if len(got) != 1 {
		t.Fatalf("home ran %d turns", len(got))
	}
	rt := got[0]
	if rt.Peer != desk.svc.ID() || rt.Name != "desk" || !rt.Private || rt.Content != "how much disk?" {
		t.Errorf("remote turn %+v", rt)
	}
	for _, name := range []string{"config_set", "upgrade", "pkg_install", AdminToolName, SendToolName, "job_start", "room"} {
		if rt.Allowed(name) {
			t.Errorf("%s is allowed to another agent", name)
		}
	}
	if !rt.Allowed("exec") || !rt.Allowed(tools.AskToolName) {
		t.Error("the default grant withholds the shell")
	}
	noInbound(t, desk.bus)

	// A second message from the same chat continues the same context.
	call(t, sendTo(desk), telegram, map[string]any{"agent": "home", "message": "and now?"})
	if got := home.turns.turns(); got[1].Context != rt.Context {
		t.Error("a follow-up started a new context")
	}
	// A wider room starts a new one.
	shared := telegram
	shared.Audience = tools.AudienceShared
	call(t, sendTo(desk), shared, map[string]any{"agent": "home", "message": "in company"})
	if got := home.turns.turns(); got[2].Context == rt.Context || got[2].Private {
		t.Error("a shared room reused the private context")
	}
}

func TestQuestionThenAnswer(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	home.turns.run = func(ctx context.Context, rt agent.RemoteTurn) (string, error) {
		ans, err := rt.Ask.Ask(ctx, tools.Question{Prompt: "which report?", Options: []string{"property", "tax"}})
		if err != nil {
			return "", err
		}
		return "built the " + ans.Text + " report", nil
	}
	res := call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "build the report"})
	if !strings.Contains(res.ForLLM, "which report?") {
		t.Fatalf("result %q", res.ForLLM)
	}
	_, task, ok := strings.Cut(res.ForLLM, `task="`)
	if !ok {
		t.Fatalf("no task id in %q", res.ForLLM)
	}
	task, _, _ = strings.Cut(task, `"`)
	res = call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "property", "task": task})
	if !strings.Contains(res.ForLLM, "built the property report") {
		t.Fatalf("answer result %q", res.ForLLM)
	}
	if res := call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "x", "task": "nope"}); !res.IsError {
		t.Error("an answer to an unknown task went out")
	}
}

func TestSlowWorkReportsOnce(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	release := make(chan struct{})
	home.turns.run = func(ctx context.Context, rt agent.RemoteTurn) (string, error) {
		<-release
		return "the report is ready", nil
	}
	res := call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "build it"})
	if !strings.Contains(res.ForLLM, "Passed to home box") {
		t.Fatalf("result %q", res.ForLLM)
	}
	if !strings.Contains(adminOf(desk).Execute(tools.WithToolContext(context.Background(), telegram),
		map[string]any{"action": "status"}).ForLLM, "working") {
		t.Error("status does not show the work in progress")
	}
	close(release)
	msg := inbound(t, desk.bus)
	if !msg.System || msg.Channel != "telegram" || msg.ChatID != "42" || !strings.Contains(msg.Content, "the report is ready") {
		t.Errorf("delivered %+v", msg)
	}
	noInbound(t, desk.bus)
	if !strings.Contains(adminOf(desk).Execute(tools.WithToolContext(context.Background(), telegram),
		map[string]any{"action": "status"}).ForLLM, "done") {
		t.Error("status does not show the finished work")
	}
}

func TestPrivateResultWaitsForAnEmptyRoom(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	release := make(chan struct{})
	home.turns.run = func(context.Context, agent.RemoteTurn) (string, error) {
		<-release
		return "your passport number is X", nil
	}
	call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "find my passport number"})
	desk.turns.setAudience(tools.AudienceShared)
	close(release)
	held := inbound(t, desk.bus)
	if strings.Contains(held.Content, "passport number is") || !strings.Contains(held.Content, "private details") {
		t.Fatalf("delivered in company: %q", held.Content)
	}
	desk.svc.Start()
	noInbound(t, desk.bus)
	desk.turns.setAudience("")
	if msg := inbound(t, desk.bus); !strings.Contains(msg.Content, "passport number is X") {
		t.Errorf("after the room emptied: %q", msg.Content)
	}
}

func TestBackgroundQuestion(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	release := make(chan struct{})
	home.turns.run = func(ctx context.Context, rt agent.RemoteTurn) (string, error) {
		<-release
		ans, err := rt.Ask.Ask(ctx, tools.Question{Prompt: "overwrite it?"})
		if err != nil {
			return "", err
		}
		return "answer was " + ans.Text, nil
	}
	call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "save it"})
	close(release)
	msg := inbound(t, desk.bus)
	if !strings.Contains(msg.Content, "overwrite it?") || !strings.Contains(msg.Content, "task=") {
		t.Fatalf("question delivered as %q", msg.Content)
	}
}

func TestWorkerRestartFailsTheTask(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	started := make(chan struct{})
	home.turns.run = func(ctx context.Context, rt agent.RemoteTurn) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	done := make(chan *tools.Result, 1)
	hold := telegram
	go func() {
		done <- call(t, sendTo(desk), hold, map[string]any{"agent": "home box", "message": "long job"})
	}()
	<-started
	home.svc.server.Close()
	res := <-done
	if !strings.Contains(res.ForLLM, "restarted") {
		t.Errorf("result %q", res.ForLLM)
	}
}

func TestCallerRestartReconciles(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	release := make(chan struct{})
	home.turns.run = func(context.Context, agent.RemoteTurn) (string, error) {
		<-release
		return "finished while you were away", nil
	}
	call(t, sendTo(desk), telegram, map[string]any{"agent": "home box", "message": "long job"})
	// The caller goes down with the task still running.
	desk.svc.Close()
	again, err := New(desk.svc.cfg, desk.dir, "test", desk.turns, desk.bus)
	if err != nil {
		t.Fatal(err)
	}
	again.SetTransport(desk.svc.transport)
	t.Cleanup(again.Close)
	again.Start()
	close(release)
	if msg := inbound(t, desk.bus); !strings.Contains(msg.Content, "finished while you were away") {
		t.Errorf("reconciled as %q", msg.Content)
	}
}

func TestForwardingRules(t *testing.T) {
	desk, home, nas := newNode(t, "desk"), newNode(t, "home"), newNode(t, "nas")
	pair(t, map[string]*node{"desk": desk, "home box": home, "nas": nas})
	remote := tools.ToolContext{Channel: "a2a", ChatID: "x:y", SessionKey: "a2a:x:y", Trigger: "a2a"}

	// A task that arrived without the extension cannot be passed on.
	if res := call(t, sendTo(home), remote, map[string]any{"agent": "nas", "message": "x"}); !res.IsError {
		t.Error("forwarded a task with no chain")
	}
	// Sending the work back to where it came from is a loop.
	ctx := context.WithValue(tools.WithToolContext(context.Background(), remote), chainKey{},
		fa2a.Meta{Chain: []string{desk.svc.ID()}, Hops: 1})
	if res := sendTo(home).Execute(ctx, map[string]any{"agent": "desk", "message": "x"}); !res.IsError ||
		!strings.Contains(res.ForLLM, "loop") {
		t.Errorf("B→A: %q", res.ForLLM)
	}
	// Onward to a third agent, the chain grows and the turn waits.
	nas.turns.run = func(context.Context, agent.RemoteTurn) (string, error) { return "nas says hi", nil }
	if res := sendTo(home).Execute(ctx, map[string]any{"agent": "nas", "message": "x"}); res.IsError ||
		!strings.Contains(res.ForLLM, "nas says hi") {
		t.Errorf("onward: %q", res.ForLLM)
	}
	// Too many hops is refused.
	far := context.WithValue(tools.WithToolContext(context.Background(), remote), chainKey{},
		fa2a.Meta{Chain: []string{"a", "b", "c"}, Hops: fa2a.MaxHops})
	if res := sendTo(home).Execute(far, map[string]any{"agent": "nas", "message": "x"}); !res.IsError {
		t.Error("forwarded past the hop limit")
	}
	// The grant decides whether swarm_send is offered at all.
	if allowed(Grant{}, true)(SendToolName) || !allowed(Grant{Forward: true}, true)(SendToolName) ||
		allowed(Grant{Forward: true}, false)(SendToolName) {
		t.Error("forwarding grant")
	}
	if allowed(Grant{Tools: []string{"read_file"}}, true)("exec") || !allowed(Grant{Tools: []string{"read_file"}}, true)("read_file") {
		t.Error("tool grant")
	}
}

func TestRunRefusesStrangersAndStaleRosters(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home box": home})
	if _, err := home.svc.Run(context.Background(), fa2a.Turn{Peer: "stranger"}); err == nil {
		t.Error("a stranger's task ran")
	}
	_ = home.svc.book.Update(func(st *State) error {
		st.Refreshed = time.Now().Add(-StaleAfter - time.Hour)
		return nil
	})
	if _, err := home.svc.Run(context.Background(), fa2a.Turn{Peer: desk.svc.ID()}); err == nil ||
		!strings.Contains(err.Error(), "confirm") {
		t.Errorf("stale roster: %v", err)
	}
	_ = home.svc.book.Update(func(st *State) error {
		st.Refreshed = time.Now()
		return nil
	})
	home.turns.run = func(context.Context, agent.RemoteTurn) (string, error) { return "  ", nil }
	if _, err := home.svc.Run(context.Background(), fa2a.Turn{Peer: desk.svc.ID()}); err == nil {
		t.Error("an empty answer counted as one")
	}
}

func TestFailureReasons(t *testing.T) {
	cases := map[error]string{
		&cost.BudgetError{Scope: "member", Spent: 1, Limit: 1}:          "daily cap",
		&provider.APIError{Status: 402, Reason: provider.ReasonBilling}: "402",
		context.Canceled:   "cancelled",
		errors.New("boom"): "boom",
	}
	for err, want := range cases {
		if got := failure(err).Error(); !strings.Contains(got, want) {
			t.Errorf("failure(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestAdminTool(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	admin := adminOf(desk)
	ctx := tools.WithToolContext(context.Background(), telegram)
	if res := admin.Execute(ctx, map[string]any{"action": "list"}); !strings.Contains(res.ForLLM, "not in a swarm") {
		t.Errorf("list before joining: %q", res.ForLLM)
	}
	if res := call(t, sendTo(desk), telegram, map[string]any{"agent": "x", "message": "y"}); !res.IsError {
		t.Error("sent without a swarm")
	}
	pair(t, map[string]*node{"desk": desk, "home box": home})
	if res := admin.Execute(ctx, map[string]any{"action": "list"}); !strings.Contains(res.ForLLM, "home box") {
		t.Errorf("list: %q", res.ForLLM)
	}
	if res := admin.Execute(ctx, map[string]any{"action": "rename", "agent": "home box", "name": "nas"}); res.IsError {
		t.Errorf("rename: %q", res.ForLLM)
	}
	if res := admin.Execute(ctx, map[string]any{"action": "grant", "agent": "nas",
		"tools": []any{"read_file"}, "forward": true}); res.IsError || !strings.Contains(res.ForLLM, "only: read_file") {
		t.Errorf("grant: %q", res.ForLLM)
	}
	if g := desk.svc.book.Read().Peer(home.svc.ID()).Grant; !g.Forward || len(g.Tools) != 1 {
		t.Errorf("grant saved as %+v", g)
	}
	job := telegram
	job.Trigger = "job"
	for _, action := range []string{"grant", "invite", "join", "remove"} {
		res := adminOf(desk).Execute(tools.WithToolContext(context.Background(), job), map[string]any{"action": action, "agent": "nas"})
		if !res.IsError || !strings.Contains(res.ForLLM, "only a turn the user started") {
			t.Errorf("%s from a job: %q", action, res.ForLLM)
		}
	}
	if res := admin.Execute(ctx, map[string]any{"action": "status"}); !strings.Contains(res.ForLLM, "Nothing has been handed") {
		t.Errorf("status: %q", res.ForLLM)
	}
	if res := admin.Execute(ctx, map[string]any{"action": "dance"}); !res.IsError {
		t.Error("unknown action")
	}
	if MemberOf("a2a:peer:ctx") != "peer" || MemberOf("telegram:1") != "" {
		t.Error("MemberOf")
	}
}

func TestToolsDescribeThemselves(t *testing.T) {
	n := newNode(t, "desk")
	for _, tool := range n.svc.Tools() {
		if tool.Name() == "" || len(tool.Description()) < 40 || tool.Parameters()["type"] != "object" {
			t.Errorf("%s is not described", tool.Name())
		}
	}
	if n.svc.Identity().ID != n.svc.ID() || n.svc.Book() == nil || n.svc.Name() != "desk" || !strings.HasSuffix(Dir(), "swarm") {
		t.Error("accessors")
	}
	if _, ok := n.svc.tlsPeer(httptest.NewRequest("GET", "/", nil)); ok {
		t.Error("a request without TLS named a peer")
	}
}
