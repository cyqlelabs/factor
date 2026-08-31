// Package trace records what a turn actually did.
//
// Factor already knows all of this and keeps none of it. Phase changes go to
// one live watcher for the status line and are dropped; the registry runs
// every tool and remembers only the ones that panicked; the meter sees every
// model call and stores two integers. So "it answered as the wrong person",
// "it went quiet for ninety seconds" and "why did that turn cost so much" are
// reconstructed from memory rather than read.
//
// A trace is the trajectory: what came in, which models answered, which tools
// ran and for how long, what they cost, and how the turn ended. It is the
// record three things need — a person debugging, the control bands watching
// for drift, and any eval built from real work rather than imagination.
//
// It stays deliberately small. This is a single-user agent on the user's own
// machine, so the trace is a local file with a retention limit, it never
// leaves the box, and it records what a tool was called rather than what was
// said to it unless asked. Tool arguments hold the user's file paths, their
// searches and the things they asked to be remembered; the questions this
// record exists to answer need the shape of a turn, not its contents.
package trace

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record is one turn, written as a single JSON line when the turn ends.
type Record struct {
	ID       string    `json:"id"`
	Started  time.Time `json:"started"`
	Session  string    `json:"session"`
	Channel  string    `json:"channel"`
	Trigger  string    `json:"trigger,omitempty"` // user | cron | job | heartbeat
	Speaker  string    `json:"speaker,omitempty"`
	Duration float64   `json:"duration_s"`

	Models []ModelCall `json:"models,omitempty"`
	Tools  []ToolCall  `json:"tools,omitempty"`
	Events []Event     `json:"events,omitempty"`

	// Outcome is how the turn ended: "ok", "error", "interrupted", "budget".
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`

	InputTokens  int     `json:"input_tokens,omitempty"`
	CachedTokens int     `json:"cached_input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	USD          float64 `json:"usd,omitempty"`
}

// ModelCall is one provider round trip.
type ModelCall struct {
	Model    string  `json:"model"`
	Input    int     `json:"input"`
	Cached   int     `json:"cached,omitempty"`
	Written  int     `json:"cache_write,omitempty"`
	Output   int     `json:"output"`
	USD      float64 `json:"usd,omitempty"`
	Duration float64 `json:"duration_s,omitempty"`
}

// ToolCall is one tool execution: what ran, for how long, how much it
// returned, and whether it failed.
type ToolCall struct {
	Name     string   `json:"name"`
	Duration float64  `json:"duration_s"`
	Bytes    int      `json:"bytes"`
	Error    bool     `json:"error,omitempty"`
	ArgKeys  []string `json:"arg_keys,omitempty"`
	// Args is present only when trace.record_args is on. It holds the
	// arguments after the same secret filter tool results pass through,
	// bounded, because a browser call can carry a page of them.
	Args string `json:"args,omitempty"`
}

// Event is something that happened to the turn rather than in it: a provider
// failover, a compaction, a message steered in mid-flight, a budget refusal,
// a user correcting the answer.
type Event struct {
	At     float64 `json:"at_s"`
	Kind   string  `json:"kind"`
	Detail string  `json:"detail,omitempty"`
}

// Event kinds. Named rather than free text because the control bands count
// them and a typo would read as a metric that never fires.
const (
	EventFailover   = "failover"
	EventCompaction = "compaction"
	EventSteering   = "steering"
	EventBudget     = "budget"
	EventOverflow   = "overflow"
	EventBargeIn    = "barge_in"
	EventForget     = "forget"
	EventAsk        = "ask"
)

// maxArgChars bounds a recorded argument blob. Enough to tell one call from
// another, far short of a page.
const maxArgChars = 400

// Config is what the user gets to decide.
type Config struct {
	Enabled bool
	// RecordArgs adds argument values to each tool call. Off by default: the
	// shape of a turn answers the questions traces exist for, and the values
	// are the user's paths, searches and secrets-adjacent text.
	RecordArgs bool
	KeepDays   int
}

// Recorder writes turn records and prunes old ones. Every method is safe on a
// nil receiver, so the loop calls them unconditionally and a disabled tracer
// costs a nil check.
type Recorder struct {
	dir    string
	cfg    Config
	filter func(string) string

	mu   sync.Mutex
	open map[string]*Turn // session key -> the turn currently running there
	day  string
	file *os.File
}

// NewRecorder opens the trace directory, or returns nil when tracing is off.
// filter is the same secret scrubber the tool registry uses; nil means none.
func NewRecorder(dir string, cfg Config, filter func(string) string) *Recorder {
	if !cfg.Enabled {
		return nil
	}
	if cfg.KeepDays <= 0 {
		cfg.KeepDays = 14
	}
	if filter == nil {
		filter = func(s string) string { return s }
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("tracing disabled: cannot create the trace directory", "dir", dir, "error", err)
		return nil
	}
	r := &Recorder{dir: dir, cfg: cfg, filter: filter, open: map[string]*Turn{}}
	r.prune()
	return r
}

// Dir is where records are written, for the readers that scan them.
func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// Turn is one in-flight turn. Methods are safe on a nil receiver.
type Turn struct {
	rec  *Recorder
	mu   sync.Mutex
	data Record
}

// Begin opens a turn. The returned handle is nil when tracing is off.
func (r *Recorder) Begin(sessionKey, trigger, speaker string) *Turn {
	if r == nil {
		return nil
	}
	channel, _, _ := strings.Cut(sessionKey, ":")
	t := &Turn{rec: r, data: Record{
		ID:      fmt.Sprintf("%d-%s", time.Now().UnixNano(), sanitize(channel)),
		Started: time.Now(),
		Session: sessionKey,
		Channel: channel,
		Trigger: trigger,
		Speaker: speaker,
	}}
	r.mu.Lock()
	r.open[sessionKey] = t
	r.mu.Unlock()
	return t
}

// Tool records one execution.
func (t *Turn) Tool(name string, args map[string]any, d time.Duration, bytes int, isErr bool) {
	if t == nil {
		return
	}
	call := ToolCall{
		Name:     name,
		Duration: d.Seconds(),
		Bytes:    bytes,
		Error:    isErr,
		ArgKeys:  argKeys(args),
	}
	if t.rec.cfg.RecordArgs {
		call.Args = t.rec.renderArgs(args)
	}
	t.mu.Lock()
	t.data.Tools = append(t.data.Tools, call)
	t.mu.Unlock()
}

// Event records something that happened to the turn.
func (t *Turn) Event(kind, detail string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.data.Events = append(t.data.Events, Event{
		At:     time.Since(t.data.Started).Seconds(),
		Kind:   kind,
		Detail: detail,
	})
	t.mu.Unlock()
}

// Charge attributes one priced model call to the turn running on a session.
// Calls with no turn open — an idle compaction, an induction verdict — are
// written as records of their own, because spend nobody asked for is exactly
// the spend worth being able to find.
func (r *Recorder) Charge(sessionKey string, call ModelCall) {
	if r == nil {
		return
	}
	r.mu.Lock()
	t := r.open[sessionKey]
	r.mu.Unlock()
	if t == nil {
		r.write(Record{
			ID: fmt.Sprintf("%d-housekeeping", time.Now().UnixNano()), Started: time.Now(),
			Session: sessionKey, Trigger: "housekeeping", Outcome: "ok",
			Models: []ModelCall{call}, InputTokens: call.Input, CachedTokens: call.Cached,
			OutputTokens: call.Output, USD: call.USD,
		})
		return
	}
	t.mu.Lock()
	t.data.Models = append(t.data.Models, call)
	t.data.InputTokens += call.Input
	t.data.CachedTokens += call.Cached
	t.data.OutputTokens += call.Output
	t.data.USD += call.USD
	t.mu.Unlock()
}

// Event records something against whatever turn is running on a session, for
// callers that know the session but not the turn — the provider chain failing
// over, a channel reporting that the user talked over the answer. Nothing is
// recorded when no turn is open: an event with no turn to belong to is noise.
func (r *Recorder) Event(sessionKey, kind, detail string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	t := r.open[sessionKey]
	r.mu.Unlock()
	t.Event(kind, detail)
}

// End closes the turn and writes it.
func (t *Turn) End(outcome string, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.data.Duration = time.Since(t.data.Started).Seconds()
	t.data.Outcome = outcome
	if err != nil {
		t.data.Error = t.rec.filter(err.Error())
	}
	rec := t.data
	t.mu.Unlock()

	t.rec.mu.Lock()
	if t.rec.open[rec.Session] == t {
		delete(t.rec.open, rec.Session)
	}
	t.rec.mu.Unlock()

	t.rec.write(rec)
}

// write appends one record to the day's file, rotating at midnight.
func (r *Recorder) write(rec Record) {
	raw, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("could not encode a trace record", "error", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	day := rec.Started.Format("2006-01-02")
	if r.file == nil || r.day != day {
		if r.file != nil {
			_ = r.file.Close()
		}
		f, ferr := os.OpenFile(filepath.Join(r.dir, day+".jsonl"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if ferr != nil {
			slog.Warn("could not open the trace file", "error", ferr)
			return
		}
		r.file, r.day = f, day
		go r.pruneAsync()
	}
	if _, werr := r.file.Write(append(raw, '\n')); werr != nil {
		slog.Warn("could not write a trace record", "error", werr)
	}
}

// Close releases the day's file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *Recorder) pruneAsync() { r.prune() }

// prune drops the days past the retention limit. A trace is worth keeping
// while it can still explain something recent; older than that it is a log
// nobody reads growing on a machine nobody is watching.
func (r *Recorder) prune() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	var days []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			days = append(days, e.Name())
		}
	}
	if len(days) <= r.cfg.KeepDays {
		return
	}
	sort.Strings(days) // ISO dates sort chronologically
	for _, name := range days[:len(days)-r.cfg.KeepDays] {
		if err := os.Remove(filepath.Join(r.dir, name)); err != nil {
			slog.Warn("could not prune a trace file", "file", name, "error", err)
		}
	}
}

// renderArgs writes the arguments through the secret filter, bounded.
func (r *Recorder) renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	s := r.filter(string(raw))
	if len(s) > maxArgChars {
		s = s[:maxArgChars] + "…"
	}
	return s
}

// argKeys names what a call was given without saying what it said, which is
// enough to tell a read from a write.
func argKeys(args map[string]any) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, 0, len(args))
	for k := range args {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sanitize keeps a trace id filename-safe even though it is only ever a field.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}
