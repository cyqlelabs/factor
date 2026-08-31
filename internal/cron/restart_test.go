package cron

// What happens when a restart lands on top of a scheduled job. An upgrade, a
// config change and a reboot all stop the scheduler at a moment nobody chose,
// and the minute a job is due is as likely as any other. Each test here pins
// one of those overlaps: a job still running when the process is told to stop,
// a job the stop cuts short, a job whose store could not be written on the way
// out, and a morning of missed ticks caught up across two restarts.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedClock is a moment the test chooses and may move while the scheduler is
// reading it.
type fixedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fixedClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// recorder collects deliveries, which arrive on the job's own goroutine.
type recorder struct {
	mu   sync.Mutex
	sent []string
}

func (r *recorder) deliver(_, _, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, content)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

// serviceAt opens the store in dir with the clock parked at a moment. Jobs
// already on disk are loaded, which is what makes a second call the process
// that comes up after a restart.
func serviceAt(t *testing.T, dir string, at time.Time, handler Handler, deliver Deliver) (*Service, *fixedClock) {
	t.Helper()
	s, err := NewService(dir, handler, deliver)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{at: at}
	s.now = clock.now
	return s, clock
}

// runInBackground starts the scheduler and returns a stop that waits for it,
// the way a restart waits for the process it is replacing.
func runInBackground(t *testing.T, s *Service) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("the scheduler never stopped")
		}
	}
}

const (
	beforeNine = "2026-01-01T08:59:30Z"
	justAfter  = "2026-01-01T09:00:05Z"
)

func moment(t *testing.T, stamp string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// An upgrade cancels the scheduler's context, and a job already running has to
// be allowed to finish: Run stops after it, not during it. Otherwise the
// process exits mid-turn and the answer the user was waiting for goes nowhere.
func TestRunWaitsForAJobStillRunningWhenTheProcessIsToldToStop(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var rec recorder
	handler := func(context.Context, Job) (string, error) {
		close(entered)
		<-release // still working when the restart is requested
		return "the brief", nil
	}
	s, clock := serviceAt(t, t.TempDir(), moment(t, beforeNine), handler, rec.deliver)
	if _, err := s.Add("0 9 * * *", "morning brief", "telegram", "77"); err != nil {
		t.Fatal(err)
	}
	clock.set(moment(t, justAfter))

	stop := runInBackground(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the due job never started")
	}

	stopped := make(chan struct{})
	go func() { defer close(stopped); stop() }() // the upgrade lands here
	select {
	case <-stopped:
		t.Fatal("the scheduler stopped while one of its jobs was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the scheduler never stopped after its last job finished")
	}
	if got := rec.all(); len(got) != 1 || got[0] != "the brief" {
		t.Errorf("the answer of a job that outlived the restart request = %v", got)
	}
}

// The other half: a turn the restart really does cut short. The failure is
// delivered as a sentence rather than swallowed, and the process that comes up
// does not run the tick again — it was dispatched, and the same reminder
// arriving twice is worse than one that said it went wrong.
func TestAJobCutShortByTheRestartIsReportedAndNotRunAgain(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	var rec recorder
	handler := func(ctx context.Context, _ Job) (string, error) {
		close(entered)
		<-ctx.Done() // the turn dies with the process
		return "", ctx.Err()
	}
	s, clock := serviceAt(t, dir, moment(t, beforeNine), handler, rec.deliver)
	if _, err := s.Add("0 9 * * *", "morning brief", "telegram", "77"); err != nil {
		t.Fatal(err)
	}
	clock.set(moment(t, justAfter))

	stop := runInBackground(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the due job never started")
	}
	stop()

	sent := rec.all()
	if len(sent) != 1 || !strings.Contains(sent[0], "failed") {
		t.Fatalf("a turn the restart killed must still reach the user: %v", sent)
	}

	// The process that comes up seconds later, still inside the same minute.
	after, _ := serviceAt(t, dir, moment(t, "2026-01-01T09:00:40Z"), nil, nil)
	if due := after.dueJobs(); len(due) != 0 {
		t.Errorf("the restart replayed a tick that had already been dispatched: %+v", due)
	}
}

// A store that cannot be written on the way out — a full disk, a home gone
// read-only — leaves a dispatched one-off on disk, so the process that comes up
// delivers it a second time. That is the trade on purpose: a reminder that
// arrives twice is a nuisance, one that never arrives is the defect this
// scheduler exists to prevent.
func TestAOneOffThatCouldNotBeDeletedFiresAgainAfterARestart(t *testing.T) {
	dir := t.TempDir()
	s, clock := serviceAt(t, dir, moment(t, beforeNine), nil, nil)
	if _, err := s.AddOnce(clock.now().Add(time.Minute), "the kettle is on", "telegram", "77"); err != nil {
		t.Fatal(err)
	}

	s.path = brokenPath(t) // the write that deletes it cannot land
	clock.set(clock.now().Add(2 * time.Minute))
	if due := s.dueJobs(); len(due) != 1 {
		t.Fatalf("the one-off did not fire: %+v", due)
	}
	if left := s.List(); len(left) != 0 {
		t.Errorf("a dispatched one-off is gone from memory whatever disk did: %+v", left)
	}

	after, _ := serviceAt(t, dir, clock.now(), nil, nil)
	due := after.dueJobs()
	if len(due) != 1 || due[0].Message != "the kettle is on" {
		t.Fatalf("the reminder went down with the write that failed: %+v", due)
	}
	// And the process whose store does work finishes the job off.
	if left := after.List(); len(left) != 0 {
		t.Errorf("the one-off outlived a run that could be saved: %+v", left)
	}
}

// A restart during a catch-up: the scheduler wakes owing a whole morning of
// ticks, runs them, and is restarted again minutes later. The second start owes
// nothing — an upgrade restarts twice in a row often enough (the binary, then
// the config the new version rewrote) that replaying here would double every
// reminder.
func TestASecondRestartDoesNotReplayTheCatchUp(t *testing.T) {
	dir := t.TempDir()
	s, clock := serviceAt(t, dir, moment(t, "2026-01-01T08:00:00Z"), nil, nil)
	for _, schedule := range []string{"0 9 * * *", "30 9 * * *"} {
		if _, err := s.Add(schedule, "brief at "+schedule, "telegram", "77"); err != nil {
			t.Fatal(err)
		}
	}

	// Down all morning, back at ten with both ticks owed.
	clock.set(moment(t, "2026-01-01T10:00:00Z"))
	if due := s.dueJobs(); len(due) != 2 {
		t.Fatalf("a morning of missed ticks caught up %d jobs, want 2: %+v", len(due), due)
	}
	if due := s.dueJobs(); len(due) != 0 {
		t.Errorf("the catch-up is owed all over again immediately: %+v", due)
	}

	after, _ := serviceAt(t, dir, moment(t, "2026-01-01T10:05:00Z"), nil, nil)
	if due := after.dueJobs(); len(due) != 0 {
		t.Errorf("a second restart replayed the catch-up: %+v", due)
	}
	// Tomorrow, though, both are owed again: catching up is not cancelling.
	tomorrow, _ := serviceAt(t, dir, moment(t, "2026-01-02T09:31:00Z"), nil, nil)
	if due := tomorrow.dueJobs(); len(due) != 2 {
		t.Errorf("the catch-up swallowed the next day's runs: %+v", due)
	}
}

// A panic in a scheduled turn used to take the whole gateway with it — every
// channel, every other job — and a one-off with it, since the store no longer
// holds it. It is now the failure of that one job, said out loud.
func TestAPanickingJobIsReportedRatherThanFatal(t *testing.T) {
	var rec recorder
	handler := func(context.Context, Job) (string, error) {
		panic("a tool dereferenced nothing")
	}
	s, clock := serviceAt(t, t.TempDir(), moment(t, beforeNine), handler, rec.deliver)
	if _, err := s.AddOnce(clock.now().Add(time.Minute), "the kettle is on", "telegram", "77"); err != nil {
		t.Fatal(err)
	}
	clock.set(clock.now().Add(2 * time.Minute))

	stop := runInBackground(t, s)
	waitFor(t, func() bool { return len(rec.all()) == 1 }, "the failure of the panicking job")
	stop()

	said := rec.all()[0]
	for _, want := range []string{"the kettle is on", "dereferenced nothing", "one-off"} {
		if !strings.Contains(said, want) {
			t.Errorf("the report of a crashed reminder does not say %q: %s", want, said)
		}
	}
}

// A failure names the reminder, not just its id. For a one-off the store no
// longer holds the message, so this sentence is the only copy left of what the
// user asked for.
func TestAFailureNamesTheReminderAndWhetherItWillBeTriedAgain(t *testing.T) {
	recurring := Job{ID: "cron-1", Schedule: "0 9 * * *", Message: "post the weekly report"}
	text := failureText(recurring, errors.New("the provider is unreachable"))
	if !strings.Contains(text, "post the weekly report") || !strings.Contains(text, "unreachable") {
		t.Errorf("failure text = %q", text)
	}
	if strings.Contains(text, "one-off") {
		t.Errorf("a recurring job was reported as gone for good: %q", text)
	}

	once := Job{ID: "cron-2", At: time.Now(), Message: "tell me the kettle is on"}
	if text := failureText(once, errors.New("boom")); !strings.Contains(text, "one-off") {
		t.Errorf("a one-off failure does not say it will not be tried again: %q", text)
	}

	long := Job{ID: "cron-3", Message: strings.Repeat("x", 400)}
	if len(failureText(long, errors.New("boom"))) > 400 {
		t.Error("a long prompt was quoted back whole")
	}
}

// The shutdown has to be able to wait for a turn it just cancelled: the answer
// still has to reach the pump that carries it, and the pump stops with the
// channels.
func TestWaitBlocksWhileAJobIsStillRunning(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	handler := func(context.Context, Job) (string, error) {
		close(entered)
		<-release
		return "", nil
	}
	s, clock := serviceAt(t, t.TempDir(), moment(t, beforeNine), handler, nil)
	if _, err := s.Add("0 9 * * *", "morning brief", "telegram", "77"); err != nil {
		t.Fatal(err)
	}
	clock.set(moment(t, justAfter))
	if !s.Idle() {
		t.Fatal("a scheduler that has run nothing is idle")
	}

	stop := runInBackground(t, s)
	defer stop()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the due job never started")
	}
	if s.Idle() {
		t.Error("a scheduler with a job in flight reported itself idle")
	}

	// A wait shorter than the job gives up rather than holding the shutdown
	// open for ever.
	started := time.Now()
	s.Wait(50 * time.Millisecond)
	if waited := time.Since(started); waited > time.Second {
		t.Errorf("Wait ignored its timeout: %s", waited)
	}

	close(release)
	s.Wait(3 * time.Second)
	if !s.Idle() {
		t.Error("Wait returned with a job still running")
	}
}

// waitFor polls until the condition holds, or says what never happened.
func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never happened", what)
}
