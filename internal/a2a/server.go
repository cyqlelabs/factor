package a2a

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
)

// restPrefix is where the HTTP+JSON binding is served.
const restPrefix = "/rest"

// Turn is one task, handed to the Runner.
type Turn struct {
	// Peer is the caller's agent id, as the connection proved it.
	Peer    string
	Context string
	Task    string
	Text    string
	// Meta is what the caller said about its room and the chain of agents
	// behind it; HasMeta is false when it said nothing.
	Meta    Meta
	HasMeta bool
	// Ask puts a question back to the caller and waits for the answer. The
	// task shows INPUT_REQUIRED until one arrives.
	Ask func(ctx context.Context, question string, options []string) (string, error)
}

// Runner runs a task as a turn and returns its answer. An error fails the
// task, and its text is the reason the caller reads.
type Runner interface {
	Run(ctx context.Context, t Turn) (string, error)
}

// Options bound what one machine takes on.
type Options struct {
	// Self is this agent's id, refused in a caller's chain: work that has
	// already passed through here is a loop.
	Self string
	// Concurrent tasks run at once, and Queue more may wait for a slot.
	Concurrent int
	Queue      int
	// FreeMB reports memory available on the machine; under MinFreeMB a new
	// task is refused rather than run into swap. Nil skips the check.
	FreeMB    func() int
	MinFreeMB int
	Card      *a2a.AgentCard
	// Push delivers push notifications, where the transport can carry them.
	// Nil leaves them off.
	Push push.Sender
}

// Server runs tasks other agents send. A turn runs on a context the server
// owns, not on the request's: the SDK cancels the context that produced a
// task the moment it asks a question, and the turn has to survive that to
// hear the answer.
type Server struct {
	base   context.Context
	cancel context.CancelFunc
	runner Runner
	store  *Store
	opts   Options

	slots   chan struct{}
	waiting atomic.Int32

	mu   sync.Mutex
	live map[a2a.TaskID]*liveTask
	wg   sync.WaitGroup
}

// NewServer builds a server whose turns live until Close.
func NewServer(runner Runner, store *Store, opts Options) *Server {
	if opts.Concurrent <= 0 {
		opts.Concurrent = 1
	}
	base, cancel := context.WithCancel(context.Background())
	return &Server{base: base, cancel: cancel, runner: runner, store: store, opts: opts,
		slots: make(chan struct{}, opts.Concurrent), live: map[a2a.TaskID]*liveTask{}}
}

// Close cancels every task still running and waits for their turns to end.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

// CancelPeer ends every task an agent has running here, which removing it
// from the swarm calls for.
func (s *Server) CancelPeer(peer string) {
	s.mu.Lock()
	var cancel []*liveTask
	for _, lt := range s.live {
		if lt.peer == peer {
			cancel = append(cancel, lt)
		}
	}
	s.mu.Unlock()
	for _, lt := range cancel {
		lt.mu.Lock()
		lt.failure = "the agent that sent this task was removed from the swarm"
		lt.mu.Unlock()
		lt.cancel()
	}
}

// Running is how many tasks have a turn in memory, waiting ones included.
func (s *Server) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

type peerKey struct{}

// Handler serves the agent card and JSON-RPC. peerOf names the agent behind
// a request from what the connection proved — a pinned certificate — and a
// request it cannot name is refused before the protocol sees it.
func (s *Server) Handler(peerOf func(*http.Request) (string, bool)) http.Handler {
	opts := []a2asrv.RequestHandlerOption{a2asrv.WithTaskStore(s.store), a2asrv.WithCallInterceptors(identify{})}
	if s.opts.Push != nil {
		opts = append(opts, a2asrv.WithPushNotifications(push.NewInMemoryStore(), s.opts.Push))
	}
	handler := a2asrv.NewHandler(s, opts...)
	mux := http.NewServeMux()
	if s.opts.Card != nil {
		card := *s.opts.Card
		card.Capabilities.PushNotifications = s.opts.Push != nil
		mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(&card))
	}
	// Both of A2A's HTTP bindings: JSON-RPC at the root, HTTP+JSON under
	// /rest, which the card names as a second interface.
	mux.Handle(restPrefix+"/", http.StripPrefix(restPrefix, a2asrv.NewRESTHandler(handler)))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(handler))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, ok := peerOf(r)
		if !ok || peer == "" {
			http.Error(w, "unknown agent", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerKey{}, peer)))
	})
}

// identify carries the peer the connection proved onto the call, where the
// task store reads its owner from.
type identify struct {
	a2asrv.PassthroughCallInterceptor
}

func (identify) Before(ctx context.Context, cc *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	peer, _ := ctx.Value(peerKey{}).(string)
	if peer == "" {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	cc.User = a2asrv.NewAuthenticatedUser(peer, nil)
	return ctx, nil, nil
}

type eventKind int

const (
	evQuestion eventKind = iota
	evDone
	evFailed
)

type event struct {
	kind    eventKind
	text    string
	options []string
}

// liveTask is a task whose turn is in memory. While it waits for an answer
// nothing executes it, so anything that happens then is written to the store
// directly.
type liveTask struct {
	id      a2a.TaskID
	ctxID   string
	peer    string
	cancel  context.CancelFunc
	events  chan event
	answers chan string

	mu        sync.Mutex
	attached  bool
	asking    bool
	cancelled bool
	failure   string
}

func (lt *liveTask) attach(on bool) {
	lt.mu.Lock()
	lt.attached = on
	lt.mu.Unlock()
}

func agentText(info a2a.TaskInfoProvider, text string) *a2a.Message {
	return a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart(text))
}

// Execute implements a2asrv.AgentExecutor.
func (s *Server) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		peer := ""
		if ec.User != nil && ec.User.Authenticated {
			peer = ec.User.Name
		}
		if peer == "" {
			yield(nil, a2a.ErrUnauthenticated)
			return
		}
		if ec.StoredTask == nil {
			s.start(ctx, ec, peer, yield)
			return
		}
		s.resume(ctx, ec, peer, yield)
	}
}

// Cancel implements a2asrv.AgentExecutor.
func (s *Server) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if lt := s.task(ec.TaskID); lt != nil {
			lt.mu.Lock()
			lt.cancelled = true
			lt.mu.Unlock()
			lt.cancel()
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, agentText(ec, "the task was cancelled")), nil)
	}
}

func (s *Server) task(id a2a.TaskID) *liveTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[id]
}

func (s *Server) refusal(ec *a2asrv.ExecutorContext) string {
	meta, ok := MetaOf(ec.Message)
	if ok && s.opts.Self != "" && meta.Loops(s.opts.Self) {
		return "this work has already passed through this agent; sending it back would be a loop"
	}
	if ok && meta.Hops > MaxHops {
		return fmt.Sprintf("this work has passed through %d agents, more than the %d allowed", meta.Hops, MaxHops)
	}
	if s.opts.FreeMB != nil && s.opts.MinFreeMB > 0 {
		if free := s.opts.FreeMB(); free >= 0 && free < s.opts.MinFreeMB {
			return fmt.Sprintf("this machine has %d MB of memory free, under the %d MB a task needs", free, s.opts.MinFreeMB)
		}
	}
	return ""
}

// admit takes a slot, waiting in the queue if there is room in it. It
// reports false, with the reason, when the task cannot run here now.
func (s *Server) admit(ctx context.Context) (bool, string) {
	select {
	case s.slots <- struct{}{}:
		return true, ""
	default:
	}
	if int(s.waiting.Add(1)) > s.opts.Queue {
		s.waiting.Add(-1)
		return false, fmt.Sprintf("this agent is busy: %d task(s) running and %d waiting", s.opts.Concurrent, s.opts.Queue)
	}
	defer s.waiting.Add(-1)
	select {
	case s.slots <- struct{}{}:
		return true, ""
	case <-ctx.Done():
		return false, "the request ended while it waited for a slot"
	case <-s.base.Done():
		return false, "this agent is shutting down"
	}
}

func (s *Server) start(ctx context.Context, ec *a2asrv.ExecutorContext, peer string, yield func(a2a.Event, error) bool) {
	if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
		return
	}
	if reason := s.refusal(ec); reason != "" {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateRejected, agentText(ec, reason)), nil)
		return
	}
	if ok, reason := s.admit(ctx); !ok {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateRejected, agentText(ec, reason)), nil)
		return
	}
	turnCtx, cancel := context.WithCancel(s.base)
	lt := &liveTask{id: ec.TaskID, ctxID: ec.ContextID, peer: peer, cancel: cancel,
		events: make(chan event, 1), answers: make(chan string, 1), attached: true}
	s.mu.Lock()
	s.live[ec.TaskID] = lt
	s.mu.Unlock()

	meta, hasMeta := MetaOf(ec.Message)
	turn := Turn{Peer: peer, Context: ec.ContextID, Task: string(ec.TaskID), Text: TextOf(ec.Message),
		Meta: meta, HasMeta: hasMeta,
		Ask: func(ctx context.Context, q string, options []string) (string, error) {
			return s.ask(ctx, lt, q, options)
		}}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots }()
		defer cancel()
		reply, err := s.runner.Run(turnCtx, turn)
		s.finish(lt, reply, err)
	}()

	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		lt.attach(false)
		return
	}
	s.pump(ctx, ec, lt, yield)
}

func (s *Server) resume(ctx context.Context, ec *a2asrv.ExecutorContext, peer string, yield func(a2a.Event, error) bool) {
	lt := s.task(ec.TaskID)
	if lt == nil || lt.peer != peer {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed,
			agentText(ec, "this task is no longer running here: the agent restarted or the task already ended")), nil)
		return
	}
	lt.mu.Lock()
	asking := lt.asking
	if asking {
		lt.attached = true
	}
	lt.mu.Unlock()
	if !asking {
		yield(nil, fmt.Errorf("%w: the task is not waiting for an answer", a2a.ErrInvalidRequest))
		return
	}
	select {
	case lt.answers <- TextOf(ec.Message):
	default:
		lt.attach(false)
		yield(nil, fmt.Errorf("%w: the question has already been answered", a2a.ErrInvalidRequest))
		return
	}
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		lt.attach(false)
		return
	}
	s.pump(ctx, ec, lt, yield)
}

// pump turns what the turn does into events until the task asks, ends, or
// the execution is cancelled.
func (s *Server) pump(ctx context.Context, ec *a2asrv.ExecutorContext, lt *liveTask, yield func(a2a.Event, error) bool) {
	defer lt.attach(false)
	select {
	case ev := <-lt.events:
		switch ev.kind {
		case evQuestion:
			msg := agentText(ec, ev.text)
			if len(ev.options) > 0 {
				msg.SetMeta(ExtensionURI, map[string]any{"options": ev.options})
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, msg), nil)
		case evDone:
			if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(ev.text)), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, agentText(ec, ev.text)), nil)
		case evFailed:
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, agentText(ec, ev.text)), nil)
		}
	case <-ctx.Done():
	}
}

// emit hands an event to the execution attached to the task, or writes it
// to the store when none is: a turn can end while its caller is away
// deciding what to answer.
func (s *Server) emit(lt *liveTask, ev event) {
	lt.mu.Lock()
	attached := lt.attached
	lt.mu.Unlock()
	if attached {
		lt.events <- ev
		return
	}
	state := map[eventKind]a2a.TaskState{evQuestion: a2a.TaskStateInputRequired,
		evDone: a2a.TaskStateCompleted, evFailed: a2a.TaskStateFailed}[ev.kind]
	if err := s.store.setStatus(lt.id, state, ev.text); err != nil {
		slog.Warn("a task's outcome could not be stored", "task", lt.id, "error", err)
	}
}

func (s *Server) ask(ctx context.Context, lt *liveTask, question string, options []string) (string, error) {
	lt.mu.Lock()
	if lt.asking {
		lt.mu.Unlock()
		return "", errors.New("a question is already waiting for an answer")
	}
	lt.asking = true
	lt.mu.Unlock()
	defer func() {
		lt.mu.Lock()
		lt.asking = false
		lt.mu.Unlock()
	}()
	s.emit(lt, event{kind: evQuestion, text: question, options: options})
	started := time.Now()
	select {
	case answer := <-lt.answers:
		return answer, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Nobody answered in time. The turn cannot go on guessing on the
			// other agent's behalf, so the task fails with that reason.
			lt.mu.Lock()
			lt.failure = fmt.Sprintf("the question went unanswered for %s", time.Since(started).Round(time.Second))
			lt.mu.Unlock()
			lt.cancel()
		}
		return "", ctx.Err()
	}
}

func (s *Server) finish(lt *liveTask, reply string, err error) {
	s.mu.Lock()
	delete(s.live, lt.id)
	s.mu.Unlock()
	lt.mu.Lock()
	cancelled, failure := lt.cancelled, lt.failure
	lt.mu.Unlock()
	switch {
	case cancelled:
		// Cancel already wrote the outcome.
	case failure != "":
		s.emit(lt, event{kind: evFailed, text: failure})
	case err != nil && s.base.Err() != nil:
		s.emit(lt, event{kind: evFailed, text: restartReason})
	case err != nil:
		s.emit(lt, event{kind: evFailed, text: err.Error()})
	default:
		s.emit(lt, event{kind: evDone, text: reply})
	}
}
