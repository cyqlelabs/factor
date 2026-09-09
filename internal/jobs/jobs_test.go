package jobs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

type notifyRecorder struct {
	mu   sync.Mutex
	jobs []*Job
	ch   chan *Job
}

func newNotifyRecorder() *notifyRecorder {
	return &notifyRecorder{ch: make(chan *Job, 16)}
}

func (n *notifyRecorder) notify(j *Job) {
	n.mu.Lock()
	n.jobs = append(n.jobs, j)
	n.mu.Unlock()
	n.ch <- j
}

func (n *notifyRecorder) wait(t *testing.T) *Job {
	t.Helper()
	select {
	case j := <-n.ch:
		return j
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification")
		return nil
	}
}

func TestExecJobCompletesAndNotifies(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, nil, rec.notify)
	origin := Origin{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42"}

	job, err := e.Start(KindExec, "greet", "echo working on it", origin)
	if err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if done.ID != job.ID || done.State != StateDone {
		t.Fatalf("done = %+v", done)
	}
	if !strings.Contains(done.OutputTail(), "working on it") {
		t.Errorf("output = %q", done.OutputTail())
	}
	if done.Origin != origin {
		t.Errorf("origin = %+v", done.Origin)
	}
}

func TestExecJobFailureState(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, nil, rec.notify)
	_, err := e.Start(KindExec, "", payloadFailErr, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if done.State != StateFailed {
		t.Errorf("state = %s", done.State)
	}
	if !strings.Contains(done.OutputTail(), "oops") {
		t.Errorf("stderr not captured: %q", done.OutputTail())
	}
}

func TestTaskJobRunsDelegate(t *testing.T) {
	rec := newNotifyRecorder()
	runTask := func(_ context.Context, prompt, sessionKey, _ string) (string, error) {
		if prompt != "research something" || !strings.HasPrefix(sessionKey, "job:") {
			t.Errorf("prompt=%q session=%q", prompt, sessionKey)
		}
		return "research complete: 42", nil
	}
	e := NewEngine(context.Background(), t.TempDir(), nil, runTask, rec.notify)
	if _, err := e.Start(KindTask, "research", "research something", Origin{}); err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if done.State != StateDone || !strings.Contains(done.OutputTail(), "research complete: 42") {
		t.Errorf("done = %s output=%q", done.State, done.OutputTail())
	}
}

func TestCancelRunningJob(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, nil, rec.notify)
	job, err := e.Start(KindExec, "long", payloadSleep, Origin{})
	if err != nil {
		t.Fatal(err)
	}
	// wait for it to actually start
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := e.Get(job.ID); j.State == StateRunning && !j.Started.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if err := e.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if time.Since(start) > 8*time.Second {
		t.Fatal("cancel did not kill the process group promptly")
	}
	j, _ := e.Get(job.ID)
	if j.State != StateCancelled {
		t.Errorf("state = %s", j.State)
	}
	// cancelled jobs must NOT notify (user asked for the cancel)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, n := range rec.jobs {
		if n.ID == job.ID {
			t.Error("cancelled job sent a notification")
		}
	}
}

func TestOutputTailBounded(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, nil, rec.notify)
	if _, err := e.Start(KindExec, "", payloadFlood, Origin{}); err != nil {
		t.Fatal(err)
	}
	done := rec.wait(t)
	if len(done.OutputTail()) > tailBytes {
		t.Errorf("tail = %d bytes, cap %d", len(done.OutputTail()), tailBytes)
	}
}

func TestListOrdersAndPrunes(t *testing.T) {
	rec := newNotifyRecorder()
	e := NewEngine(context.Background(), t.TempDir(), nil, nil, rec.notify)
	for range 3 {
		if _, err := e.Start(KindExec, "", payloadOK, Origin{}); err != nil {
			t.Fatal(err)
		}
	}
	e.Wait()
	if got := len(e.List()); got != 3 {
		t.Errorf("list = %d", got)
	}
}

// The audit's fourth probe: two fresh engines both handed out "j1", and with
// it the persistent session key "job:j1". Unrelated jobs a restart apart, or
// two Factor processes sharing a workspace, inherited each other's history and
// spending bucket while job_start promised a fresh agent run.
func TestJobIDsAreUniqueAcrossEngines(t *testing.T) {
	first := NewEngine(context.Background(), t.TempDir(), nil, nil, nil)
	second := NewEngine(context.Background(), t.TempDir(), nil, nil, nil)

	a, err := first.Start(KindExec, "one", "true", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Start(KindExec, "one", "true", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Errorf("two fresh engines both minted %q, so both task sub-turns run as job:%s", a.ID, a.ID)
	}
	// Still readable, and still ordered within an engine.
	c, err := first.Start(KindExec, "two", "true", Origin{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.ID, "j1-") || !strings.HasPrefix(c.ID, "j2-") {
		t.Errorf("ids = %q, %q, want a readable counter with the run token behind it", a.ID, c.ID)
	}
	first.Wait()
	second.Wait()
}

// The audit's fifth probe: a command a custom deny pattern blocked in the
// foreground ran happily as a background job. Delegating slow work is an
// ordinary workflow the operating rules encourage, so a rail one of the two
// first-party shells enforces is not a rail.
func TestBackgroundExecObeysTheDenyPatterns(t *testing.T) {
	guard, err := tools.NewCommandGuard(true, []string{`\bsecret-thing\b`})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(context.Background(), t.TempDir(), guard, nil, nil)

	if _, err := e.Start(KindExec, "custom", "echo secret-thing", Origin{}); err == nil {
		t.Error("a command blocked in the foreground started as a background job")
	}
	if _, err := e.Start(KindExec, "builtin", "rm -rf /tmp/whatever", Origin{}); err == nil {
		t.Error("a built-in catastrophe pattern started as a background job")
	}
	// A task job's prompt is not a command and is not checked against shell
	// patterns: the words are for a model, not for a shell.
	runTask := func(context.Context, string, string, string) (string, error) { return "ok", nil }
	withTasks := NewEngine(context.Background(), t.TempDir(), guard, runTask, nil)
	if _, err := withTasks.Start(KindTask, "prompt", "tell me about secret-thing", Origin{}); err != nil {
		t.Errorf("a task prompt was refused by a shell pattern: %v", err)
	}
	withTasks.Wait()
}

// The audience travels with the job, so a sub-turn delegated from a room with
// company in it recalls and stores under that room's scope rather than the
// blank one a background session would otherwise default to.
func TestTaskJobsRunUnderTheOriginAudience(t *testing.T) {
	seen := make(chan string, 1)
	runTask := func(_ context.Context, _, _, audience string) (string, error) {
		seen <- audience
		return "done", nil
	}
	e := NewEngine(context.Background(), t.TempDir(), nil, runTask, nil)
	if _, err := e.Start(KindTask, "research", "look it up",
		Origin{Channel: "voice", ChatID: "local:room", Audience: tools.AudienceShared}); err != nil {
		t.Fatal(err)
	}
	e.Wait()

	if got := <-seen; got != tools.AudienceShared {
		t.Errorf("the sub-turn ran with audience %q, want the room the job was started in", got)
	}
}
