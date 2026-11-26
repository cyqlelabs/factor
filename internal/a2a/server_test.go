package a2a

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// runnerFunc adapts a function to Runner.
type runnerFunc func(ctx context.Context, t Turn) (string, error)

func (f runnerFunc) Run(ctx context.Context, t Turn) (string, error) { return f(ctx, t) }

// asPeer stamps every request with the agent id a test connection proves.
type asPeer struct {
	id   string
	next http.RoundTripper
}

func (p asPeer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Peer", p.id)
	return p.next.RoundTrip(r)
}

func testPeerOf(r *http.Request) (string, bool) {
	id := r.Header.Get("X-Test-Peer")
	return id, id != ""
}

type rig struct {
	t      *testing.T
	srv    *Server
	http   *httptest.Server
	store  *Store
	dir    string
	runner runnerFunc
}

func newRig(t *testing.T, runner runnerFunc, opts Options) *rig {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir, KeepTasks)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Self == "" {
		opts.Self = "worker"
	}
	srv := NewServer(runner, store, opts)
	hs := httptest.NewServer(srv.Handler(testPeerOf))
	t.Cleanup(func() {
		hs.Close()
		srv.Close()
	})
	return &rig{t: t, srv: srv, http: hs, store: store, dir: dir, runner: runner}
}

func (r *rig) client(peer string) *a2aclient.Client {
	r.t.Helper()
	hc := &http.Client{Transport: asPeer{id: peer, next: http.DefaultTransport}}
	cl, err := Dial(context.Background(), hc, r.http.URL)
	if err != nil {
		r.t.Fatal(err)
	}
	return cl
}

func send(t *testing.T, cl *a2aclient.Client, msg *a2a.Message) *a2a.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cl.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("result is %T, want a task", res)
	}
	return task
}

func text(s string) *a2a.Message { return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(s)) }

func TestDelegateAndDone(t *testing.T) {
	var got Turn
	r := newRig(t, func(_ context.Context, turn Turn) (string, error) {
		got = turn
		return "the disk has 40 GB free", nil
	}, Options{})
	msg := text("how much disk?")
	Meta{Audience: AudiencePrivate, Chain: []string{"origin"}, Hops: 1}.Attach(msg)
	task := send(t, r.client("desk"), msg)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state %s, want completed", task.Status.State)
	}
	if ResultText(task) != "the disk has 40 GB free" {
		t.Errorf("result %q", ResultText(task))
	}
	if got.Peer != "desk" || got.Text != "how much disk?" || !got.HasMeta || !got.Meta.Private() || got.Context == "" || got.Task == "" {
		t.Errorf("turn = %+v", got)
	}
}

func TestQuestionAndAnswer(t *testing.T) {
	r := newRig(t, func(ctx context.Context, turn Turn) (string, error) {
		answer, err := turn.Ask(ctx, "which report?", []string{"property", "tax"})
		if err != nil {
			return "", err
		}
		return "built the " + answer + " report", nil
	}, Options{})
	cl := r.client("desk")
	task := send(t, cl, text("build the report"))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state %s, want input-required", task.Status.State)
	}
	if q := TextOf(task.Status.Message); q != "which report?" {
		t.Errorf("question %q", q)
	}
	task = send(t, cl, a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("property")))
	if task.Status.State != a2a.TaskStateCompleted || ResultText(task) != "built the property report" {
		t.Fatalf("state %s result %q", task.Status.State, ResultText(task))
	}
}

func TestAnotherAgentCannotSeeOrAnswerATask(t *testing.T) {
	r := newRig(t, func(ctx context.Context, turn Turn) (string, error) {
		return turn.Ask(ctx, "yes or no?", nil)
	}, Options{})
	task := send(t, r.client("desk"), text("go"))
	other := r.client("stranger")
	if _, err := other.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("another agent's GetTask: %v, want not found", err)
	}
	_, err := other.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("yes"))})
	if err == nil {
		t.Error("another agent answered the question")
	}
	list, err := other.ListTasks(context.Background(), &a2a.ListTasksRequest{})
	if err != nil || len(list.Tasks) != 0 {
		t.Errorf("another agent lists %v (%v)", list, err)
	}
	mine, err := r.client("desk").ListTasks(context.Background(), &a2a.ListTasksRequest{ContextID: task.ContextID})
	if err != nil || len(mine.Tasks) != 1 {
		t.Errorf("owner lists %v (%v)", mine, err)
	}
}

func TestUnknownCallerIsRefused(t *testing.T) {
	r := newRig(t, func(context.Context, Turn) (string, error) { return "x", nil }, Options{})
	cl, err := Dial(context.Background(), http.DefaultClient, r.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: text("hi")}); err == nil {
		t.Error("a caller the connection cannot name got through")
	}
}

func TestBusyAgentRejects(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	r := newRig(t, func(ctx context.Context, _ Turn) (string, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "done", nil
	}, Options{Concurrent: 1, Queue: 0})
	cl := r.client("desk")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		send(t, cl, text("first"))
	}()
	<-started
	task := send(t, cl, text("second"))
	if task.Status.State != a2a.TaskStateRejected || !strings.Contains(TextOf(task.Status.Message), "busy") {
		t.Errorf("state %s %q, want rejected as busy", task.Status.State, TextOf(task.Status.Message))
	}
	close(release)
	wg.Wait()
}

func TestQueuedTaskWaitsForTheSlot(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 4)
	r := newRig(t, func(ctx context.Context, turn Turn) (string, error) {
		started <- turn.Text
		if turn.Text == "first" {
			<-release
		}
		return turn.Text + " done", nil
	}, Options{Concurrent: 1, Queue: 1})
	cl := r.client("desk")
	results := make(chan *a2a.Task, 2)
	go func() { results <- send(t, cl, text("first")) }()
	<-started
	go func() { results <- send(t, cl, text("second")) }()
	select {
	case s := <-started:
		t.Fatalf("%s started while the slot was taken", s)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if task := <-results; task.Status.State != a2a.TaskStateCompleted {
			t.Errorf("state %s", task.Status.State)
		}
	}
}

func TestLoopsHopsAndMemoryAreRefused(t *testing.T) {
	ran := false
	run := func(context.Context, Turn) (string, error) { ran = true; return "x", nil }
	free := 100
	r := newRig(t, run, Options{Self: "worker", FreeMB: func() int { return free }, MinFreeMB: 300})
	cl := r.client("desk")

	loop := text("again")
	Meta{Chain: []string{"origin", "WORKER"}, Hops: 2}.Attach(loop)
	if task := send(t, cl, loop); task.Status.State != a2a.TaskStateRejected ||
		!strings.Contains(TextOf(task.Status.Message), "loop") {
		t.Errorf("loop: %s %q", task.Status.State, TextOf(task.Status.Message))
	}
	far := text("far")
	Meta{Chain: []string{"a", "b", "c", "d"}, Hops: MaxHops + 1}.Attach(far)
	if task := send(t, cl, far); task.Status.State != a2a.TaskStateRejected {
		t.Errorf("too many hops: %s", task.Status.State)
	}
	if task := send(t, cl, text("short of memory")); task.Status.State != a2a.TaskStateRejected ||
		!strings.Contains(TextOf(task.Status.Message), "MB") {
		t.Errorf("memory: %s %q", task.Status.State, TextOf(task.Status.Message))
	}
	if ran {
		t.Error("a refused task ran")
	}
	free = 4000
	if task := send(t, cl, text("now")); task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("with memory free: %s", task.Status.State)
	}
}

func TestUnansweredQuestionFailsTheTask(t *testing.T) {
	r := newRig(t, func(ctx context.Context, turn Turn) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		return turn.Ask(ctx, "still there?", nil)
	}, Options{})
	cl := r.client("desk")
	task := send(t, cl, text("go"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := cl.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
		if err == nil && got.Status.State == a2a.TaskStateFailed {
			if !strings.Contains(TextOf(got.Status.Message), "unanswered") {
				t.Errorf("reason %q", TextOf(got.Status.Message))
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the task never failed")
}

func TestRunnerErrorIsTheReason(t *testing.T) {
	r := newRig(t, func(context.Context, Turn) (string, error) {
		return "", errors.New("the model provider refused payment (402)")
	}, Options{})
	task := send(t, r.client("desk"), text("go"))
	if task.Status.State != a2a.TaskStateFailed || !strings.Contains(TextOf(task.Status.Message), "402") {
		t.Errorf("state %s %q", task.Status.State, TextOf(task.Status.Message))
	}
}

func TestCancelEndsTheTurn(t *testing.T) {
	ended := make(chan error, 1)
	r := newRig(t, func(ctx context.Context, turn Turn) (string, error) {
		_, err := turn.Ask(ctx, "wait", nil)
		ended <- err
		return "", err
	}, Options{})
	cl := r.client("desk")
	task := send(t, cl, text("go"))
	got, err := cl.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.State != a2a.TaskStateCanceled {
		t.Errorf("state %s", got.Status.State)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("turn ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn kept running")
	}
}

func TestAnswerToATaskNoLongerRunning(t *testing.T) {
	r := newRig(t, func(ctx context.Context, turn Turn) (string, error) {
		return turn.Ask(ctx, "which?", nil)
	}, Options{})
	cl := r.client("desk")
	task := send(t, cl, text("go"))
	// The turn that was waiting is gone, as it would be after a restart.
	lt := r.srv.task(task.ID)
	r.srv.mu.Lock()
	delete(r.srv.live, task.ID)
	r.srv.mu.Unlock()
	got := send(t, cl, a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("a")))
	if got.Status.State != a2a.TaskStateFailed || !strings.Contains(TextOf(got.Status.Message), "no longer running") {
		t.Errorf("state %s %q", got.Status.State, TextOf(got.Status.Message))
	}
	lt.cancel()
}

func TestRestartFailsWhatWasRunning(t *testing.T) {
	block := make(chan struct{})
	r := newRig(t, func(ctx context.Context, turn Turn) (string, error) {
		<-ctx.Done()
		close(block)
		return "", ctx.Err()
	}, Options{})
	cl := r.client("desk")
	done := make(chan *a2a.Task, 1)
	go func() { done <- send(t, cl, text("long")) }()
	for r.srv.Running() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	r.srv.Close()
	<-block
	if task := <-done; task.Status.State != a2a.TaskStateFailed || TextOf(task.Status.Message) != restartReason {
		t.Errorf("state %s %q", task.Status.State, TextOf(task.Status.Message))
	}

	// A task left marked as running on disk is failed when the store opens.
	running := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if _, err := r.store.Create(context.Background(), running); err != nil {
		t.Fatal(err)
	}
	again, err := OpenStore(r.dir, KeepTasks)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Get(context.Background(), "t1")
	if err != nil || got.Task.Status.State != a2a.TaskStateFailed || TextOf(got.Task.Status.Message) != restartReason {
		t.Errorf("reopened: %+v %v", got, err)
	}
}

func TestStoreKeepsAWeekAndRefusesOddIDs(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "../escape"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("odd id: %v", err)
	}
	if _, err := s.Create(context.Background(), &a2a.Task{ID: "../x"}); err == nil {
		t.Error("stored a task under a path")
	}
	task := &a2a.Task{ID: "old", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	if _, err := s.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), task); err == nil {
		t.Error("created the same task twice")
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	s.mu.Lock()
	s.pruneLocked()
	s.mu.Unlock()
	if _, err := s.Get(context.Background(), "old"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("a task past the keep window survived: %v", err)
	}
}

func TestMetaHelpers(t *testing.T) {
	m := Meta{Audience: AudiencePrivate, Chain: []string{"a"}, Hops: 1}
	next := m.Next("b")
	if !next.Loops("A") || !next.Loops("b") || next.Loops("c") || next.Hops != 2 || len(m.Chain) != 1 {
		t.Errorf("next = %+v, m = %+v", next, m)
	}
	if _, ok := MetaOf(text("plain")); ok {
		t.Error("a message without the extension read as having it")
	}
	if _, ok := MetaOf(nil); ok {
		t.Error("nil message has meta")
	}
	if TextOf(nil) != "" || ResultText(nil) != "" {
		t.Error("nil text")
	}
	if !Terminal(a2a.TaskStateRejected) || Terminal(a2a.TaskStateInputRequired) {
		t.Error("terminal states")
	}
	card := Card("home box", "1.0", "https://x.swarm/")
	if card.Name != "home box" || len(card.Capabilities.Extensions) != 1 {
		t.Errorf("card %+v", card)
	}
}

func TestRESTBinding(t *testing.T) {
	r := newRig(t, func(_ context.Context, turn Turn) (string, error) { return "rest: " + turn.Text, nil },
		Options{Card: Card("w", "1", "http://x/")})
	hc := &http.Client{Transport: asPeer{id: "desk", next: http.DefaultTransport}}
	cl, err := a2aclient.NewFromEndpoints(context.Background(),
		[]*a2a.AgentInterface{a2a.NewAgentInterface(r.http.URL+restPrefix, a2a.TransportProtocolHTTPJSON)},
		a2aclient.WithRESTTransport(hc))
	if err != nil {
		t.Fatal(err)
	}
	task := send(t, cl, text("hi"))
	if task.Status.State != a2a.TaskStateCompleted || ResultText(task) != "rest: hi" {
		t.Errorf("over REST: %s %q", task.Status.State, ResultText(task))
	}
	card := Card("w", "1", "https://x.swarm/")
	if len(card.SupportedInterfaces) != 2 || card.SupportedInterfaces[1].URL != "https://x.swarm/rest" {
		t.Errorf("card interfaces %+v", card.SupportedInterfaces)
	}
}
