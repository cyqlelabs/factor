// Package jobs runs long work in the background so the agent can reply
// immediately and report back when the work finishes. Two kinds:
//
//   - exec: a shell command supervised as a process group
//   - task: a delegated agent sub-turn (research, multi-step work)
//
// On completion the engine notifies the originating session; the agent then
// proactively messages the user on the channel the job came from.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

type State string

const (
	StateRunning   State = "running"
	StateDone      State = "done"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

type Kind string

const (
	KindExec Kind = "exec"
	KindTask Kind = "task"
)

type Job struct {
	ID          string
	Kind        Kind
	Description string
	Payload     string // command or prompt
	State       State
	Started     time.Time
	Finished    time.Time
	Origin      Origin

	mu     sync.Mutex
	output *tailBuffer
	cancel context.CancelFunc
}

// Origin identifies where to deliver the completion report, and who will be
// able to hear it. Audience travels with the job because the work outlives
// the turn that asked for it: a job started in a room with company in it
// reports back into that same room, and a completion that arrived without an
// audience was answered out of the private graph in front of whoever was
// standing there.
type Origin struct {
	Channel    string
	ChatID     string
	SessionKey string
	Audience   string
}

// state reads the job's state under the job's own lock. Every mutable Job
// field is guarded by j.mu, not by the engine lock, so readers that hold only
// the engine lock (prune) must still come through here — otherwise they race
// a goroutine finishing its job.
func (j *Job) state() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.State
}

func (j *Job) OutputTail() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.output.String()
}

// View is a consistent point-in-time copy of a job's mutable state, safe to
// read while the job is running or being cancelled.
type View struct {
	ID          string
	Kind        Kind
	Description string
	State       State
	Started     time.Time
	Finished    time.Time
	Origin      Origin
}

func (j *Job) Snapshot() View {
	j.mu.Lock()
	defer j.mu.Unlock()
	return View{
		ID:          j.ID,
		Kind:        j.Kind,
		Description: j.Description,
		State:       j.State,
		Started:     j.Started,
		Finished:    j.Finished,
		Origin:      j.Origin,
	}
}

// TaskRunner runs a delegated agent sub-turn (wired to Loop.ProcessDelegated).
// The audience is the job origin's: the sub-turn recalls and stores under the
// scope of the room the work was asked for in, not the empty one a background
// session would otherwise default to.
type TaskRunner func(ctx context.Context, prompt, sessionKey, audience string) (string, error)

// Notifier receives finished jobs (wired to inject a session event).
type Notifier func(job *Job)

type Engine struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string
	seq     int
	run     string // this engine's share of every job id it mints
	workdir string

	runTask  TaskRunner
	notify   Notifier
	commands *tools.CommandGuard
	sem      chan struct{}
	wg       sync.WaitGroup
	ctx      context.Context
}

const (
	maxConcurrent = 4
	keepFinished  = 50
	execTimeLimit = 4 * time.Hour
	tailBytes     = 8 * 1024
)

// NewEngine builds the job engine. commands is the deny-list a background
// shell is checked against — the same one the foreground exec tool uses; nil
// means no patterns are configured at all.
func NewEngine(ctx context.Context, workdir string, commands *tools.CommandGuard, runTask TaskRunner, notify Notifier) *Engine {
	if notify == nil {
		notify = func(*Job) {}
	}
	return &Engine{
		jobs:     map[string]*Job{},
		run:      runToken(),
		workdir:  workdir,
		runTask:  runTask,
		notify:   notify,
		commands: commands,
		sem:      make(chan struct{}, maxConcurrent),
		ctx:      ctx,
	}
}

// runToken is what makes a job id unique beyond this engine. The counter
// alone restarts at 1 with every process, and the id is also the session key
// a task job runs its sub-turn under ("job:j1"): two unrelated jobs a restart
// apart, or two Factor processes sharing one workspace, inherited each
// other's history and spending bucket while the tool promised a fresh agent
// run. Four bytes, so the id stays something a person can read back.
func runToken() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Never observed; a clock-derived token still separates two engines
		// that were not started in the same nanosecond.
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Start launches a background job and returns immediately.
func (e *Engine) Start(kind Kind, description, payload string, origin Origin) (*Job, error) {
	if kind != KindExec && kind != KindTask {
		return nil, fmt.Errorf("unknown job kind %q (want %q or %q)", kind, KindExec, KindTask)
	}
	if strings.TrimSpace(payload) == "" {
		return nil, fmt.Errorf("empty %s payload", kind)
	}
	if kind == KindTask && e.runTask == nil {
		return nil, fmt.Errorf("task jobs are not available in this mode")
	}
	// The background shell answers to the same deny-list as the foreground
	// one. Refusing here rather than in the tool covers every caller.
	if kind == KindExec {
		if err := e.commands.Check(payload); err != nil {
			return nil, err
		}
	}

	jobCtx, cancel := context.WithCancel(e.ctx)
	e.mu.Lock()
	e.seq++
	job := &Job{
		ID:          fmt.Sprintf("j%d-%s", e.seq, e.run),
		Kind:        kind,
		Description: description,
		Payload:     payload,
		State:       StateRunning,
		Started:     time.Now(),
		Origin:      origin,
		output:      newTailBuffer(tailBytes),
		cancel:      cancel,
	}
	e.jobs[job.ID] = job
	e.order = append(e.order, job.ID)
	e.prune()
	e.mu.Unlock()

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer cancel()
		select {
		case e.sem <- struct{}{}:
			defer func() { <-e.sem }()
		case <-jobCtx.Done():
			e.finish(job, StateCancelled, nil)
			return
		}
		var err error
		switch kind {
		case KindExec:
			err = e.runExec(jobCtx, job)
		case KindTask:
			err = e.runTaskJob(jobCtx, job)
		}
		switch {
		case jobCtx.Err() != nil:
			e.finish(job, StateCancelled, err)
		case err != nil:
			e.finish(job, StateFailed, err)
		default:
			e.finish(job, StateDone, nil)
		}
	}()
	return job, nil
}

func (e *Engine) runExec(ctx context.Context, job *Job) error {
	ctx, cancel := context.WithTimeout(ctx, execTimeLimit)
	defer cancel()
	cmd := shellCommand(ctx, job.Payload)
	cmd.Dir = e.workdir
	cmd.WaitDelay = 5 * time.Second
	setProcessGroup(cmd)
	job.mu.Lock()
	cmd.Stdout, cmd.Stderr = job.output, job.output
	job.mu.Unlock()
	return cmd.Run()
}

func (e *Engine) runTaskJob(ctx context.Context, job *Job) error {
	result, err := e.runTask(ctx, job.Payload, "job:"+job.ID, job.Origin.Audience)
	if err != nil {
		fmt.Fprintf(job.output, "error: %v", err)
		return err
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	_, werr := job.output.Write([]byte(result))
	return werr
}

func (e *Engine) finish(job *Job, state State, err error) {
	job.mu.Lock()
	if job.State == StateCancelled {
		// Cancel() already delivered its verdict; the dying process's error
		// must not overwrite it or trigger a notification.
		job.mu.Unlock()
		return
	}
	job.State = state
	job.Finished = time.Now()
	if err != nil {
		fmt.Fprintf(job.output, "\n[%v]", err)
	}
	job.mu.Unlock()
	slog.Info("job finished", "id", job.ID, "state", state)
	if state != StateCancelled {
		e.notify(job)
	}
}

func (e *Engine) Get(id string) (*Job, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	return j, ok
}

func (e *Engine) List() []*Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Job, 0, len(e.order))
	for _, id := range e.order {
		out = append(out, e.jobs[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Cancel stops a running job.
func (e *Engine) Cancel(id string) error {
	job, ok := e.Get(id)
	if !ok {
		return fmt.Errorf("no job %s", id)
	}
	job.mu.Lock()
	current := job.State
	running := current == StateRunning
	if running {
		job.State = StateCancelled
		job.Finished = time.Now()
	}
	job.mu.Unlock()
	if !running {
		return fmt.Errorf("job %s already %s", id, current)
	}
	job.cancel()
	return nil
}

// prune drops the oldest finished jobs beyond keepFinished (caller holds mu).
func (e *Engine) prune() {
	finished := 0
	for _, id := range e.order {
		if e.jobs[id].state() != StateRunning {
			finished++
		}
	}
	if finished <= keepFinished {
		return
	}
	var kept []string
	for _, id := range e.order {
		if finished > keepFinished && e.jobs[id].state() != StateRunning {
			delete(e.jobs, id)
			finished--
			continue
		}
		kept = append(kept, id)
	}
	e.order = kept
}

// Wait blocks until all jobs finish (used in shutdown/tests).
func (e *Engine) Wait() { e.wg.Wait() }

// tailBuffer keeps the last N bytes written.
type tailBuffer struct {
	max  int
	data []byte
	mu   sync.Mutex
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.max {
		t.data = t.data[len(t.data)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}
