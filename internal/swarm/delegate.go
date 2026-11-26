package swarm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	fa2a "github.com/cyqlelabs/factor/internal/a2a"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Transport reaches other members.
type Transport interface {
	// HTTPClient returns a client whose connections reach the agent peer and
	// prove it is that agent, and the base URL to call it at.
	HTTPClient(peer string) (*http.Client, string, error)
}

// How long swarm_send waits for an answer before the work goes on in the
// background: a spoken reply cannot hold the line as long as a written one.
var (
	spokenWait  = 6 * time.Second
	writtenWait = 15 * time.Second
	// pollEvery is how often a task this agent lost the stream for — after a
	// restart, or a dropped connection — is asked about.
	pollEvery = 30 * time.Second
)

// Origin is the conversation a delegation reports back into.
type Origin struct {
	Channel  string `json:"channel"`
	ChatID   string `json:"chat_id"`
	Session  string `json:"session"`
	Audience string `json:"audience,omitempty"`
}

// Delegation is one message sent to another member and what came of it. The
// record is written before the message leaves, so a restart can always find
// out what happened to it.
type Delegation struct {
	ID      string `json:"id"`
	Peer    string `json:"peer"`
	Origin  Origin `json:"origin"`
	Request string `json:"request"`
	Context string `json:"context"`
	Task    string `json:"task,omitempty"`
	// Token is what the worker's pushes about this delegation carry back.
	Token   string    `json:"token,omitempty"`
	Private bool      `json:"private,omitempty"`
	State   string    `json:"state"`
	Said    string    `json:"said,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Background is set once swarm_send stopped waiting; Delivered once the
	// outcome reached the conversation; Held while a private result waits
	// for the room to empty.
	Background bool `json:"background,omitempty"`
	Delivered  bool `json:"delivered,omitempty"`
	Held       bool `json:"held,omitempty"`
}

// States a delegation can be in, in words the model reads.
const (
	stSending  = "sending"
	stWorking  = "working"
	stQuestion = "waiting for an answer"
	stDone     = "done"
	stFailed   = "failed"
	stLost     = "lost"
)

func stateWord(s a2a.TaskState) string {
	switch s {
	case a2a.TaskStateCompleted:
		return stDone
	case a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired:
		return stQuestion
	case a2a.TaskStateFailed, a2a.TaskStateRejected, a2a.TaskStateCanceled:
		return stFailed
	}
	return stWorking
}

// settled reports whether a delegation has reached something worth telling
// the user: an answer, a question, or the end.
func (d *Delegation) settled() bool {
	switch d.State {
	case stDone, stQuestion, stFailed, stLost:
		return true
	}
	return false
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "d" + hex.EncodeToString(b)
}

// ledger keeps delegations on disk, one file each.
type ledger struct {
	dir string
	mu  sync.Mutex
}

func (l *ledger) path(id string) string { return filepath.Join(l.dir, id+".json") }

func (l *ledger) save(d *Delegation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := json.Marshal(d)
	if err == nil {
		tmp := l.path(d.ID) + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, l.path(d.ID))
		}
	}
	if err != nil {
		slog.Warn("a delegation record could not be saved", "id", d.ID, "error", err)
	}
}

func (l *ledger) all() []*Delegation {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, _ := os.ReadDir(l.dir)
	var out []*Delegation
	cutoff := time.Now().Add(-fa2a.KeepTasks)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(l.dir, e.Name()))
		if err != nil {
			continue
		}
		var d Delegation
		if json.Unmarshal(data, &d) != nil {
			continue
		}
		if d.Updated.Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, e.Name()))
			continue
		}
		out = append(out, &d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// byTask is the latest delegation that carried task to peer.
func (l *ledger) byTask(peer, task string) (*Delegation, bool) {
	var found *Delegation
	for _, d := range l.all() {
		if d.Peer == peer && d.Task == task {
			found = d
		}
	}
	return found, found != nil
}

// waiter is the choice between answering in the turn that asked and
// reporting later. Whichever comes first — the outcome or the end of the
// wait — decides, under one lock, so an answer is delivered exactly once.
type waiter struct {
	mu         sync.Mutex
	outcome    *Delegation
	background bool
	ready      chan struct{}
}

// sendRequest is one swarm_send.
type sendRequest struct {
	member  Member
	message string
	task    string
	origin  Origin
	meta    fa2a.Meta
	// hold keeps the turn waiting until the outcome, for a remote turn
	// passing work on: its own caller is waiting on the same answer.
	hold bool
	wait time.Duration
}

// send delivers a message to a member and waits for what comes back, up to
// req.wait. It returns what the turn should be told.
func (s *Service) send(ctx context.Context, req sendRequest) (string, error) {
	d := &Delegation{ID: newID(), Peer: req.member.ID, Origin: req.origin, Request: req.message,
		Private: req.meta.Private(), State: stSending, Created: s.now(), Updated: s.now(), Token: newID()}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(req.message))
	if req.task != "" {
		prev, ok := s.deleg.byTask(req.member.ID, req.task)
		if !ok {
			return "", fmt.Errorf("no task %s was sent to %s from here", req.task, req.member.Name)
		}
		d.Task, d.Context, d.Private = prev.Task, prev.Context, prev.Private
		msg.TaskID, msg.ContextID = a2a.TaskID(prev.Task), prev.Context
	} else {
		ctxID, err := s.contextFor(req.origin.Session, req.member.ID, req.meta.Audience)
		if err != nil {
			return "", err
		}
		d.Context = ctxID
		msg.ContextID = ctxID
	}
	// The extension is for members: an agent outside learns nothing of who
	// asked or how the work got here.
	if !isExternal(req.member.ID) {
		req.meta.Attach(msg)
	}
	s.deleg.save(d)

	w := &waiter{ready: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.exchange(d, msg, w)
	}()

	if req.hold {
		select {
		case <-w.ready:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	timer := time.NewTimer(req.wait)
	defer timer.Stop()
	select {
	case <-w.ready:
	case <-timer.C:
	case <-ctx.Done():
	}
	// d belongs to the exchange from here on; the turn only decides.
	w.mu.Lock()
	out := w.outcome
	if out == nil {
		w.background = true
	}
	w.mu.Unlock()
	if out == nil {
		return fmt.Sprintf("Passed to %s (task %s). It is still working; you will be told once it has an answer, "+
			"so tell the user it is on its way and do not wait for it.", req.member.Name, d.ID), nil
	}
	out.Delivered = true
	out.Updated = s.now()
	s.deleg.save(out)
	return s.inlineText(out), nil
}

// exchange streams the task and settles the delegation when it reaches an
// answer, a question, or an end.
func (s *Service) exchange(d *Delegation, msg *a2a.Message, w *waiter) {
	hc, base, err := s.clientFor(d.Peer)
	if err != nil {
		s.settle(d, w, stFailed, fmt.Sprintf("could not reach it: %v", err))
		return
	}
	cl, err := fa2a.Dial(s.bg, hc, base)
	if err != nil {
		s.settle(d, w, stFailed, fmt.Sprintf("could not reach it: %v", err))
		return
	}
	defer func() { _ = cl.Destroy() }()
	var artifacts []string
	// The worker pushes what happens to the task back through the tunnel, so
	// a stream that breaks is picked up again without waiting for a poll.
	sreq := &a2a.SendMessageRequest{Message: msg}
	if !isExternal(d.Peer) {
		sreq.Config = &a2a.SendMessageConfig{PushConfig: s.pushConfig(d)}
	}
	for ev, err := range cl.SendStreamingMessage(s.bg, sreq) {
		if err != nil {
			if d.Task != "" {
				// The stream broke after the task existed: what it ends as is
				// still on the other side, and asking is how to find out.
				s.follow(d, w)
				return
			}
			s.settle(d, w, stFailed, fmt.Sprintf("the message did not get through: %v", err))
			return
		}
		switch e := ev.(type) {
		case *a2a.Task:
			d.Task, d.Context = string(e.ID), e.ContextID
			if msg.TaskID != "" && !fa2a.Terminal(e.Status.State) {
				// An answer's stream opens with the task as it stood before
				// the answer landed — still asking the question this message
				// answers.
				s.deleg.save(d)
				continue
			}
			if s.observe(d, w, e.Status, fa2a.ResultText(e)) {
				return
			}
		case *a2a.TaskStatusUpdateEvent:
			said := fa2a.TextOf(e.Status.Message)
			if e.Status.State == a2a.TaskStateCompleted && len(artifacts) > 0 {
				said = strings.Join(artifacts, "\n")
			}
			if s.observe(d, w, e.Status, said) {
				return
			}
		case *a2a.TaskArtifactUpdateEvent:
			if t := fa2a.ResultText(&a2a.Task{Artifacts: []*a2a.Artifact{e.Artifact}}); t != "" {
				artifacts = append(artifacts, t)
			}
		case *a2a.Message:
			s.settle(d, w, stDone, fa2a.TextOf(e))
			return
		}
	}
	if !d.settled() {
		s.follow(d, w)
	}
}

// observe records a status and settles on one worth reporting.
func (s *Service) observe(d *Delegation, w *waiter, st a2a.TaskStatus, said string) bool {
	word := stateWord(st.State)
	if word == stWorking {
		d.State, d.Updated = stWorking, s.now()
		if said != "" {
			d.Said = said
		}
		s.deleg.save(d)
		return false
	}
	s.settle(d, w, word, said)
	return true
}

// follow polls a task this agent has no stream for, until it settles.
func (s *Service) follow(d *Delegation, w *waiter) {
	nudged, release := s.follower(d.ID)
	defer release()
	ticker := time.NewTicker(s.pollEvery)
	defer ticker.Stop()
	for {
		if done := s.poll(d, w); done {
			return
		}
		select {
		case <-s.bg.Done():
			return
		case <-ticker.C:
		case <-nudged:
		}
	}
}

// poll asks the other agent once about a task. It reports whether the
// delegation settled.
func (s *Service) poll(d *Delegation, w *waiter) bool {
	if d.Task == "" {
		return false
	}
	hc, base, err := s.clientFor(d.Peer)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(s.bg, 10*time.Second)
	defer cancel()
	cl, err := fa2a.Dial(ctx, hc, base)
	if err != nil {
		return false
	}
	defer func() { _ = cl.Destroy() }()
	task, err := cl.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(d.Task)})
	if errors.Is(err, a2a.ErrTaskNotFound) {
		s.settle(d, w, stLost, "the other agent has no record of the task")
		return true
	}
	if err != nil {
		return false
	}
	return s.observe(d, w, task.Status, fa2a.ResultText(task))
}

// settle records where the delegation ended up and hands it to whoever is
// waiting, or, once nobody is, to the conversation that asked.
func (s *Service) settle(d *Delegation, w *waiter, state, said string) {
	d.State, d.Said, d.Updated = state, said, s.now()
	s.deleg.save(d)
	w.mu.Lock()
	if !w.background {
		out := *d
		w.outcome = &out
		close(w.ready)
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	d.Background = true
	s.deliver(d)
}

// deliver puts a settled delegation into the conversation that asked, as a
// message the agent wrote rather than the user. A result produced in private
// waits while the room it is headed for has company.
func (s *Service) deliver(d *Delegation) {
	if d.Delivered {
		return
	}
	name := s.book.Read().Label(d.Peer)
	if d.Private && s.turns.AudienceAt(d.Origin.Channel, d.Origin.ChatID) == tools.AudienceShared {
		if !d.Held {
			s.publish(d, fmt.Sprintf("[system] %s has an answer to %q, but it was produced in private and the room "+
				"now has company. Say only that you have private details for when the user is alone.", name, d.Request))
			d.Held = true
			s.deleg.save(d)
		}
		return
	}
	if s.publish(d, s.reportText(d, name)) {
		d.Delivered, d.Held, d.Updated = true, false, s.now()
		s.deleg.save(d)
	}
}

func (s *Service) publish(d *Delegation, content string) bool {
	if s.bus == nil {
		return false
	}
	return s.bus.PublishInbound(bus.InboundMessage{Channel: d.Origin.Channel, ChatID: d.Origin.ChatID,
		Content: content, Time: s.now(), Audience: d.Origin.Audience, System: true})
}

func (s *Service) reportText(d *Delegation, name string) string {
	switch d.State {
	case stDone:
		return fmt.Sprintf("[system] %s finished the task you passed it (%q). Its answer:\n%s\n\nTell the user.",
			name, d.Request, d.Said)
	case stQuestion:
		return fmt.Sprintf("[system] %s needs an answer to carry on with %q: %s\nPut this question to the user as "+
			"your reply. When they answer, call swarm_send with agent=%q, task=%q and their answer.",
			name, d.Request, d.Said, name, d.Task)
	}
	return fmt.Sprintf("[system] %s could not do %q: %s. Tell the user.", name, d.Request, d.Said)
}

func (s *Service) inlineText(d *Delegation) string {
	name := s.book.Read().Label(d.Peer)
	switch d.State {
	case stDone:
		return fmt.Sprintf("%s answered: %s", name, d.Said)
	case stQuestion:
		return fmt.Sprintf("%s needs an answer before it can go on: %s\nPut the question to the user as your reply. "+
			"When they answer, call swarm_send with agent=%q, task=%q and their answer.", name, d.Said, name, d.Task)
	}
	return fmt.Sprintf("%s could not do it: %s", name, d.Said)
}

// reconcile picks up delegations a previous process left unfinished: it
// asks about the tasks that had started, and reports the ones it can no
// longer account for.
func (s *Service) reconcile() {
	for _, d := range s.deleg.all() {
		if d.Delivered {
			continue
		}
		w := &waiter{ready: make(chan struct{}), background: true}
		switch {
		case d.settled():
			s.deliver(d)
		case d.Task == "":
			s.settle(d, w, stLost, "the message may not have reached it before this agent restarted")
		default:
			s.wg.Add(1)
			go func(d *Delegation) {
				defer s.wg.Done()
				s.follow(d, w)
			}(d)
		}
	}
}

// redeliver retries the results held back for company, once the room has
// emptied.
func (s *Service) redeliver() {
	for _, d := range s.deleg.all() {
		if d.Held && !d.Delivered {
			s.deliver(d)
		}
	}
}

// contextFor is the A2A context a conversation with a member runs in. It
// keeps the audience it started with: a wider room starts a new context, so
// private history never answers a shared one.
func (s *Service) contextFor(session, peer, audience string) (string, error) {
	key := session + "|" + peer + "|" + audience
	var ctxID string
	err := s.book.Update(func(st *State) error {
		if id, ok := st.Contexts[key]; ok {
			ctxID = id
			return nil
		}
		if st.Contexts == nil {
			st.Contexts = map[string]string{}
		}
		ctxID = a2a.NewContextID()
		st.Contexts[key] = ctxID
		return nil
	})
	return ctxID, err
}
