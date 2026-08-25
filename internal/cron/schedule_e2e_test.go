package cron

// End-to-end for reminders: everything between the sentence a user says and
// the reminder arriving back is real — the real agent loop, the real tool
// registry, the real cron tool, the real scheduler, and a real provider
// speaking the real OpenAI wire format over a local HTTP server. Only the two
// ends are simulated: the model, and the clock.
//
// The model is the interesting half. It is given no schedule and no
// arithmetic: it reads what time it is out of the turn context that actually
// crossed the wire, works out the cron expression itself, and calls the tool
// with it. So a reminder arriving here can only mean the clock reached the
// model, the expression it formed was right, the scheduler agreed, and the
// answer came back out to the chat that asked — which is the whole chain the
// user's reminder failed somewhere along.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/session"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

// What the user says, what the reminder is stored as, and what the agent says
// when the reminder fires. Kept distinct so the fake model can tell a fresh
// question from its own scheduled message arriving back.
const (
	reminderAsk  = "remind me in three minutes that the kettle is on"
	reminderBody = "Tell the user the kettle is on."
	reminderSaid = "The kettle is on."
	askAhead     = 3 * time.Minute
)

// --- the clock ---------------------------------------------------------------

// testClock is real time plus an offset the test jumps forward. Real time
// underneath is the point: the turn context tells the model what time it is
// from time.Now(), so a scheduler running on an unrelated fake clock would let
// a wrong expression pass unnoticed.
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *testClock) jump(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// impatient makes the scheduler poll fast enough for a test to watch, and puts
// the real pacing back afterwards.
func impatient(t *testing.T) {
	t.Helper()
	previous := maxSleep
	maxSleep = 5 * time.Millisecond
	t.Cleanup(func() { maxSleep = previous })
}

// --- the model ---------------------------------------------------------------

// scheduleModel is a fake LLM that can tell the time and nothing else. Asked
// for a reminder it finds the clock in the turn context, works out the cron
// expression for `ahead` minutes' time, and calls the cron tool. Woken later by
// its own scheduled message, it says the reminder out loud.
type scheduleModel struct {
	t     *testing.T
	ahead time.Duration

	mu    sync.Mutex
	once  bool             // ask for a one-off moment rather than a cron expression
	calls []map[string]any // the cron arguments it asked for, in order
}

// oneOff makes the model schedule the way a person asks for a reminder: a
// single moment, not a repeating expression. Guarded, because the server
// serving this model is already accepting when a test chooses.
func (m *scheduleModel) oneOff() {
	m.mu.Lock()
	m.once = true
	m.mu.Unlock()
}

func (m *scheduleModel) wantsOneOff() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.once
}

// clockLine pulls the "Current time:" line out of whatever crossed the wire.
// A model that cannot find it cannot schedule anything, so this failing is the
// first thing the suite reports.
func clockLine(t *testing.T, body map[string]any) time.Time {
	t.Helper()
	const marker = "Current time: "
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		text, _ := msg["content"].(string)
		_, after, found := strings.Cut(text, marker)
		if !found {
			continue
		}
		line, _, _ := strings.Cut(after, "\n")
		at, err := time.Parse("Monday 2006-01-02 15:04 MST", strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("the turn context's clock line is unparsable: %q (%v)", line, err)
		}
		return at
	}
	t.Fatal("no clock reached the model: the request carries no \"Current time:\" line")
	return time.Time{}
}

// lastMessage returns the role and text of the newest message in the request,
// which is all this model needs to know what it is being asked.
func lastMessage(body map[string]any) (role, content string) {
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		return "", ""
	}
	msg, _ := messages[len(messages)-1].(map[string]any)
	role, _ = msg["role"].(string)
	content, _ = msg["content"].(string)
	return role, content
}

func (m *scheduleModel) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.t.Errorf("decode request: %v", err)
		return
	}
	role, content := lastMessage(body)

	switch {
	case role == "tool":
		// The tool answered; wrap the turn up for the user.
		reply(w, "I'll tell you then.")
	case strings.Contains(content, reminderBody):
		// This is the scheduled turn: the job's own message came back as a
		// user turn, and the reply to it is what the user hears.
		reply(w, reminderSaid)
	case strings.Contains(content, reminderAsk):
		at := clockLine(m.t, body).Add(m.ahead)
		args := map[string]any{"action": "add", "message": reminderBody}
		if m.wantsOneOff() {
			args["at"] = at.Format("2006-01-02 15:04")
		} else {
			args["schedule"] = fmt.Sprintf("%d %d * * *", at.Minute(), at.Hour())
		}
		m.call(w, args)
	default:
		// Anything else is a scheduled message with nothing to work out: say
		// it back, so two jobs due together are told apart by what arrives.
		reply(w, content)
	}
}

func (m *scheduleModel) call(w http.ResponseWriter, args map[string]any) {
	m.mu.Lock()
	m.calls = append(m.calls, args)
	id := fmt.Sprintf("call_%d", len(m.calls))
	m.mu.Unlock()

	encoded, err := json.Marshal(args)
	if err != nil {
		m.t.Errorf("marshal args: %v", err)
		return
	}
	writeChat(w, map[string]any{
		"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": id, "type": "function",
				"function": map[string]any{"name": "cron", "arguments": string(encoded)}},
		},
	}, "tool_calls")
}

func reply(w http.ResponseWriter, text string) {
	writeChat(w, map[string]any{"role": "assistant", "content": text}, "stop")
}

func writeChat(w http.ResponseWriter, message map[string]any, finish string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

// --- the rig -----------------------------------------------------------------

type delivery struct{ channel, chatID, content string }

// rig is one factor process: its own agent loop and its own cron service, over
// a jobs.json the caller chooses — so two rigs can be pointed at one store and
// behave exactly like a gateway and a `factor chat` sharing a home.
type rig struct {
	svc       *Service
	clock     *testClock
	model     *scheduleModel
	loop      *agent.Loop
	history   *session.Store
	tool      *Tool
	delivered chan delivery
	failWith  error // when set, every scheduled turn fails instead of running
}

func newRig(t *testing.T, store string) *rig {
	t.Helper()
	r := &rig{
		clock:     &testClock{},
		model:     &scheduleModel{t: t, ahead: askAhead},
		delivered: make(chan delivery, 8),
	}
	srv := httptest.NewServer(http.HandlerFunc(r.model.serve))
	t.Cleanup(srv.Close)

	t.Setenv("FACTOR_HOME", t.TempDir()) // the loop records the last chat under it
	cfg := config.Default()
	cfg.Agent.Workspace = t.TempDir()
	cfg.Agent.MaxToolIterations = 4
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewStore(filepath.Join(cfg.Agent.Workspace, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	r.history = sessions
	registry := tools.NewRegistry(cfg.Tools.IsToolEnabled, nil)
	builder := agent.NewContextBuilder(cfg, skills.NewLoader(filepath.Join(cfg.Agent.Workspace, "skills")), nil)
	r.loop = agent.NewLoop(cfg, bus.New(), provider.NewOpenAI(srv.URL, "k", "m"),
		registry, sessions, builder, (*memory.Ambient)(nil))

	// Wired exactly as internal/app wires it: a due job is a turn in its own
	// session, and its answer is published to the chat the job came from.
	r.svc, err = NewService(store,
		func(ctx context.Context, job Job) (string, error) {
			if r.failWith != nil {
				return "", r.failWith
			}
			return r.loop.ProcessDirect(ctx, job.Message, "cron:"+job.ID)
		},
		func(channel, chatID, content string) {
			r.delivered <- delivery{channel, chatID, content}
		})
	if err != nil {
		t.Fatal(err)
	}
	r.svc.now = r.clock.now
	r.tool = &Tool{Service: r.svc}
	registry.Register(r.tool)
	return r
}

// ask puts the user's sentence through a whole agent turn, the way a Telegram
// message or a spoken utterance does.
func (r *rig) ask(t *testing.T, sentence string) string {
	t.Helper()
	out, err := r.loop.ProcessDirect(withOrigin(context.Background()), sentence, "telegram:77")
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	return out
}

// schedule runs the scheduler until the test ends.
func (r *rig) schedule(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.svc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the scheduler did not stop when its context was cancelled")
		}
	})
}

// waits for one delivery, or says what never arrived.
func (r *rig) await(t *testing.T, what string) delivery {
	t.Helper()
	select {
	case d := <-r.delivered:
		return d
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never arrived", what)
		return delivery{}
	}
}

func (r *rig) nothingFor(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case got := <-r.delivered:
		t.Fatalf("something was delivered that should not have been: %+v", got)
	case <-time.After(d):
	}
}

func withOrigin(ctx context.Context) context.Context {
	return tools.WithToolContext(ctx, tools.ToolContext{
		Channel: "telegram", ChatID: "77", SessionKey: "telegram:77",
	})
}

// --- the scenarios -----------------------------------------------------------

// The whole point, in one test: a reminder asked for in words arrives.
func TestReminderAskedInWordsArrives(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.schedule(t)

	r.ask(t, reminderAsk)

	jobs := r.svc.List()
	if len(jobs) != 1 {
		t.Fatalf("the turn scheduled %d jobs, want 1: %+v", len(jobs), jobs)
	}
	if jobs[0].Channel != "telegram" || jobs[0].ChatID != "77" {
		t.Errorf("the reminder lost the chat that asked for it: %+v", jobs[0])
	}
	r.nothingFor(t, 50*time.Millisecond) // not yet — it is three minutes off

	r.clock.jump(askAhead + time.Minute)
	got := r.await(t, "the reminder")
	if got.content != reminderSaid {
		t.Errorf("delivered %q, want %q", got.content, reminderSaid)
	}
	if got.channel != "telegram" || got.chatID != "77" {
		t.Errorf("delivered to %s:%s, want telegram:77", got.channel, got.chatID)
	}
}

// The reported bug, as a test: the gateway restarts — for a config change, an
// upgrade, a reboot — across the minute a reminder was due. It has to arrive
// late rather than not at all, because the next time this schedule comes round
// is a day away, and for the dated expression a model actually writes for
// "remind me on the 5th", a year.
func TestReminderMissedWhileTheProcessWasDownStillArrives(t *testing.T) {
	impatient(t)
	store := t.TempDir()

	before := newRig(t, store)
	before.schedule(t)
	before.ask(t, reminderAsk)
	if len(before.svc.List()) != 1 {
		t.Fatalf("nothing was scheduled: %+v", before.svc.List())
	}

	// The process goes down before the reminder is due and comes back after.
	after := newRig(t, store)
	after.clock.jump(askAhead + time.Minute)
	after.schedule(t)

	got := after.await(t, "the reminder missed during the restart")
	if got.content != reminderSaid {
		t.Errorf("delivered %q, want %q", got.content, reminderSaid)
	}
	// Once, though: a catch-up is not a backlog.
	after.nothingFor(t, 100*time.Millisecond)
}

// A tick that was never missed is not fired at startup either, or every
// restart would replay yesterday.
func TestRestartDoesNotReplayATickThatAlreadyRan(t *testing.T) {
	impatient(t)
	store := t.TempDir()

	first := newRig(t, store)
	first.schedule(t)
	first.ask(t, reminderAsk)
	first.clock.jump(askAhead + time.Minute)
	first.await(t, "the reminder")

	second := newRig(t, store)
	second.clock.jump(askAhead + 2*time.Minute) // same day, after the tick
	second.schedule(t)
	second.nothingFor(t, 200*time.Millisecond)
}

// jobs.json is one file shared by every factor process on the machine. A
// reminder asked for in `factor chat` has to be run by the gateway, which is
// the only process with a scheduler.
func TestAReminderAddedByAnotherProcessIsRun(t *testing.T) {
	impatient(t)
	store := t.TempDir()

	gateway := newRig(t, store)
	gateway.schedule(t)

	// A second process — a `factor chat` session — writes to the same store.
	cli := newRig(t, store)
	cli.ask(t, reminderAsk)

	gateway.clock.jump(askAhead + time.Minute)
	got := gateway.await(t, "a reminder added by another process")
	if got.content != reminderSaid {
		t.Errorf("delivered %q, want %q", got.content, reminderSaid)
	}
}

// And the other half of sharing a file: the scheduler must not write away what
// it never read. A gateway holding a stale map erases every job the CLI added
// the next time it saves — and reuses their ids.
func TestTheSchedulerDoesNotEraseAnotherProcessesJobs(t *testing.T) {
	impatient(t)
	store := t.TempDir()

	gateway := newRig(t, store)
	cli := newRig(t, store)

	cli.ask(t, reminderAsk)
	cliJobs := cli.svc.List()
	if len(cliJobs) != 1 {
		t.Fatalf("the cli scheduled %d jobs, want 1", len(cliJobs))
	}

	// The gateway now saves for a reason of its own.
	if _, err := gateway.svc.Add("0 9 * * *", "morning brief", "telegram", "77"); err != nil {
		t.Fatal(err)
	}

	jobs := gateway.svc.List()
	if len(jobs) != 2 {
		t.Fatalf("the gateway's save left %d jobs, want both: %+v", len(jobs), jobs)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if seen[j.ID] {
			t.Errorf("two jobs share the id %q", j.ID)
		}
		seen[j.ID] = true
	}
	if !seen[cliJobs[0].ID] {
		t.Errorf("the job added by the other process is gone: %+v", jobs)
	}

	// And it is still there for the next process to load.
	reloaded := newRig(t, store)
	if got := len(reloaded.svc.List()); got != 2 {
		t.Errorf("a fresh process loaded %d jobs from the shared store, want 2", got)
	}
}

// The defect that actually cost the user their reminder: the model wrote a
// dated expression for a time that had already passed today, and nothing ever
// said the next run was a year away.
func TestSchedulingReportsWhenTheJobWillActuallyRun(t *testing.T) {
	r := newRig(t, t.TempDir())

	// A date a month behind us: whatever today is, the next occurrence of it
	// is most of a year off.
	past := time.Now().AddDate(0, 0, -30)
	stale := fmt.Sprintf("0 12 %d %d *", past.Day(), int(past.Month()))

	res := r.tool.Execute(withOrigin(context.Background()), map[string]any{
		"action": "add", "schedule": stale, "message": "the thing I meant for today",
	})
	if res.IsError {
		t.Fatalf("add = %+v", res)
	}
	next, ok := r.svc.nextRun(r.svc.List()[0])
	if !ok {
		t.Fatal("a schedule that was accepted has no next run")
	}
	if until := time.Until(next); until < 300*24*time.Hour {
		t.Fatalf("a date 30 days behind us resolved to %v (%v away); the test is not exercising the case it means to", next, until)
	}
	if !strings.Contains(res.ForLLM, next.Format("2006-01-02")) {
		t.Errorf("add did not say when the job will run — this is how a reminder\n"+
			"meant for today silently became one for next year.\ngot: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "days") {
		t.Errorf("add did not say how far off the run is: %s", res.ForLLM)
	}

	listed := r.tool.Execute(withOrigin(context.Background()), map[string]any{"action": "list"})
	if !strings.Contains(listed.ForLLM, next.Format("2006-01-02")) {
		t.Errorf("list did not say when each job runs: %s", listed.ForLLM)
	}
}

// A schedule with no next occurrence at all is a job that will never run;
// accepting it silently is the same failure in a purer form.
func TestAScheduleThatNeverRunsIsRefused(t *testing.T) {
	r := newRig(t, t.TempDir())
	// 31 February: valid syntax, no such day, ever.
	if _, err := r.svc.Add("0 12 31 2 *", "never", "telegram", "77"); err == nil {
		t.Error("a schedule with no next occurrence was accepted")
	}
	if jobs := r.svc.List(); len(jobs) != 0 {
		t.Errorf("the refused job was stored anyway: %+v", jobs)
	}
}

// A recurring reminder keeps coming back — the model writes daily expressions
// for one-off reminders, so this is what the user actually gets.
func TestARecurringReminderArrivesAgainTheNextDay(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.schedule(t)
	r.ask(t, reminderAsk)

	r.clock.jump(askAhead + time.Minute)
	r.await(t, "the first reminder")

	r.clock.jump(24 * time.Hour)
	r.await(t, "the same reminder a day later")
}

// Turning a reminder off silences it without losing it; turning it back on
// brings it back.
func TestADisabledReminderIsSilentUntilItIsEnabledAgain(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.schedule(t)
	r.ask(t, reminderAsk)

	id := r.svc.List()[0].ID
	if res := r.tool.Execute(withOrigin(context.Background()), map[string]any{
		"action": "disable", "id": id}); res.IsError {
		t.Fatalf("disable = %+v", res)
	}
	r.clock.jump(askAhead + time.Minute)
	r.nothingFor(t, 200*time.Millisecond)

	if res := r.tool.Execute(withOrigin(context.Background()), map[string]any{
		"action": "enable", "id": id}); res.IsError {
		t.Fatalf("enable = %+v", res)
	}
	r.clock.jump(24 * time.Hour)
	r.await(t, "the re-enabled reminder")
}

// Removing one is final.
func TestARemovedReminderNeverArrives(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.schedule(t)
	r.ask(t, reminderAsk)

	if res := r.tool.Execute(withOrigin(context.Background()), map[string]any{
		"action": "remove", "id": r.svc.List()[0].ID}); res.IsError {
		t.Fatalf("remove = %+v", res)
	}
	r.clock.jump(askAhead + time.Minute)
	r.nothingFor(t, 200*time.Millisecond)
}

// Two reminders due in the same minute both arrive: one is not allowed to
// shadow the other.
func TestTwoRemindersDueInTheSameMinuteBothArrive(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.schedule(t)

	at := time.Now().Add(askAhead)
	sched := fmt.Sprintf("%d %d * * *", at.Minute(), at.Hour())
	for _, msg := range []string{"first", "second"} {
		if _, err := r.svc.Add(sched, msg, "telegram", "77"); err != nil {
			t.Fatal(err)
		}
	}
	r.clock.jump(askAhead + time.Minute)

	got := map[string]bool{}
	for range 2 {
		got[r.await(t, "one of two reminders due together").content] = true
	}
	if len(got) != 2 {
		t.Errorf("the two reminders produced %d distinct deliveries: %v", len(got), got)
	}
}

// When the turn behind a reminder fails, the user is told rather than left
// waiting: a silent scheduler is indistinguishable from a broken one.
func TestAFailedScheduledTurnStillReachesTheUser(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.failWith = fmt.Errorf("the provider is unreachable")
	r.schedule(t)

	at := time.Now().Add(askAhead)
	job, err := r.svc.Add(fmt.Sprintf("%d %d * * *", at.Minute(), at.Hour()),
		"the kettle", "telegram", "77")
	if err != nil {
		t.Fatal(err)
	}
	r.clock.jump(askAhead + time.Minute)

	got := r.await(t, "the failure notice")
	if !strings.Contains(got.content, job.ID) || !strings.Contains(got.content, "unreachable") {
		t.Errorf("failure notice = %q, want the job id and the cause", got.content)
	}
}

// A scheduled turn runs in its own session, so a reminder never lands in the
// middle of the conversation the user is having.
func TestAScheduledTurnRunsInItsOwnSession(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.schedule(t)
	r.ask(t, reminderAsk)

	id := r.svc.List()[0].ID
	r.clock.jump(askAhead + time.Minute)
	r.await(t, "the reminder")

	// The asking conversation holds the question and the answer to it; the
	// reminder is somewhere else entirely.
	asking, err := r.history.History("telegram:77")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range asking {
		if strings.Contains(m.Content, reminderSaid) {
			t.Errorf("the scheduled turn wrote into the user's own session: %q", m.Content)
		}
	}
	scheduled, err := r.history.History("cron:" + id)
	if err != nil {
		t.Fatal(err)
	}
	if len(scheduled) == 0 {
		t.Fatalf("the scheduled turn left no history under cron:%s", id)
	}
}

// --- one-off reminders -------------------------------------------------------

// What a person actually asks for. The model names a moment, the reminder
// arrives once, and the job is gone afterwards — not left to come round again
// next year, which is all a cron expression can offer.
func TestAOneOffReminderArrivesOnceAndDeletesItself(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.model.oneOff()
	r.schedule(t)

	r.ask(t, reminderAsk)

	jobs := r.svc.List()
	if len(jobs) != 1 {
		t.Fatalf("the turn scheduled %d jobs, want 1: %+v", len(jobs), jobs)
	}
	if !jobs[0].Once() {
		t.Fatalf("the reminder was stored as recurring: %+v", jobs[0])
	}
	if jobs[0].Schedule != "" {
		t.Errorf("a one-off carries a cron expression too: %+v", jobs[0])
	}

	r.clock.jump(askAhead + time.Minute)
	got := r.await(t, "the one-off reminder")
	if got.content != reminderSaid {
		t.Errorf("delivered %q, want %q", got.content, reminderSaid)
	}

	// Gone from the store, and gone for good: a year from now it stays quiet.
	if left := r.svc.List(); len(left) != 0 {
		t.Errorf("the one-off is still scheduled after running: %+v", left)
	}
	r.clock.jump(366 * 24 * time.Hour)
	r.nothingFor(t, 200*time.Millisecond)
}

// A one-off missed while the process was down still arrives, for the same
// reason a recurring one does — more so, since it has no second chance at all.
func TestAOneOffMissedWhileTheProcessWasDownStillArrives(t *testing.T) {
	impatient(t)
	store := t.TempDir()

	before := newRig(t, store)
	before.model.oneOff()
	before.schedule(t)
	before.ask(t, reminderAsk)

	after := newRig(t, store)
	after.clock.jump(askAhead + time.Minute)
	after.schedule(t)

	if got := after.await(t, "the one-off missed during the restart"); got.content != reminderSaid {
		t.Errorf("delivered %q, want %q", got.content, reminderSaid)
	}
	after.nothingFor(t, 100*time.Millisecond)
}

// The mistake that started all this, refused at the door: a moment that has
// already gone by is a reminder worked out against the wrong day, and firing
// it instantly or filing it for next year are both worse than saying so.
func TestAOneOffInThePastIsRefused(t *testing.T) {
	r := newRig(t, t.TempDir())
	ctx := withOrigin(context.Background())

	res := r.tool.Execute(ctx, map[string]any{
		"action": "add", "message": "the thing I meant for this morning",
		"at": time.Now().Add(-2 * time.Hour).Format("2006-01-02 15:04"),
	})
	if !res.IsError {
		t.Fatalf("a moment in the past was accepted: %+v", res)
	}
	if !strings.Contains(res.ForLLM, "already gone by") {
		t.Errorf("the refusal does not say what is wrong: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "now") {
		t.Errorf("the refusal does not say what time it is, so it cannot be corrected: %s", res.ForLLM)
	}
	if jobs := r.svc.List(); len(jobs) != 0 {
		t.Errorf("the refused reminder was stored anyway: %+v", jobs)
	}
}

// Both fields at once means the caller does not know which it meant.
func TestAddRefusesAMomentAndAScheduleTogether(t *testing.T) {
	r := newRig(t, t.TempDir())
	res := r.tool.Execute(withOrigin(context.Background()), map[string]any{
		"action": "add", "message": "which is it",
		"at": time.Now().Add(time.Hour).Format("2006-01-02 15:04"), "schedule": "0 9 * * *",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "not both") {
		t.Errorf("add = %+v", res)
	}
	if res := r.tool.Execute(withOrigin(context.Background()), map[string]any{
		"action": "add", "message": "when?"}); !res.IsError {
		t.Errorf("add with neither a moment nor a schedule succeeded: %+v", res)
	}
	// A readable moment with nothing to say at it is still not a reminder.
	if res := r.tool.Execute(withOrigin(context.Background()), map[string]any{
		"action": "add", "at": time.Now().Add(time.Hour).Format("2006-01-02 15:04"),
	}); !res.IsError {
		t.Errorf("add with a moment but no message succeeded: %+v", res)
	}
}

// The listing has to tell the two kinds apart, or a user cannot see which of
// their reminders is going to come back.
func TestListTellsOneOffsAndRecurringApart(t *testing.T) {
	r := newRig(t, t.TempDir())
	ctx := withOrigin(context.Background())
	if res := r.tool.Execute(ctx, map[string]any{"action": "add", "message": "once only",
		"at": time.Now().Add(time.Hour).Format("2006-01-02 15:04")}); res.IsError {
		t.Fatalf("add once: %+v", res)
	}
	if res := r.tool.Execute(ctx, map[string]any{"action": "add", "message": "every day",
		"schedule": "0 9 * * *"}); res.IsError {
		t.Fatalf("add recurring: %+v", res)
	}

	listed := r.tool.Execute(ctx, map[string]any{"action": "list"}).ForLLM
	if !strings.Contains(listed, "once") {
		t.Errorf("the listing does not mark the one-off: %s", listed)
	}
	if !strings.Contains(listed, "0 9 * * *") {
		t.Errorf("the listing does not show the recurring schedule: %s", listed)
	}
}

// A one-off survives the store being shared, and being reloaded from disk with
// its moment intact.
func TestAOneOffSurvivesAReloadFromDisk(t *testing.T) {
	store := t.TempDir()
	first := newRig(t, store)
	at := time.Now().Add(90 * time.Minute).Truncate(time.Minute)
	if _, err := first.svc.AddOnce(at, "still here", "telegram", "77"); err != nil {
		t.Fatal(err)
	}

	reloaded := newRig(t, store).svc.List()
	if len(reloaded) != 1 || !reloaded[0].Once() {
		t.Fatalf("reloaded = %+v", reloaded)
	}
	if !reloaded[0].At.Equal(at) {
		t.Errorf("the moment came back as %v, want %v", reloaded[0].At, at)
	}
}

// Turning a one-off off holds it rather than losing it.
func TestADisabledOneOffDoesNotRun(t *testing.T) {
	impatient(t)
	r := newRig(t, t.TempDir())
	r.schedule(t)
	job, err := r.svc.AddOnce(time.Now().Add(askAhead), "held", "telegram", "77")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.SetEnabled(job.ID, false); err != nil {
		t.Fatal(err)
	}
	r.clock.jump(askAhead + time.Minute)
	r.nothingFor(t, 200*time.Millisecond)
	if jobs := r.svc.List(); len(jobs) != 1 {
		t.Errorf("a disabled one-off was dropped rather than held: %+v", jobs)
	}
}
