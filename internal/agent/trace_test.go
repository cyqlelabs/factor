package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/trace"
)

func tracedHarness(t *testing.T, script ...func(*provider.Request) (*provider.Response, error)) (*harness, string) {
	t.Helper()
	h := newHarness(t, script...)
	dir := t.TempDir()
	rec := trace.NewRecorder(dir, trace.Config{Enabled: true, KeepDays: 3}, nil)
	if rec == nil {
		t.Fatal("recorder should be enabled")
	}
	t.Cleanup(func() { _ = rec.Close() })
	h.loop.WithTracer(rec)
	return h, dir
}

func readTraces(t *testing.T, dir string) []trace.Record {
	t.Helper()
	recs, err := trace.Since(dir, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// The point of the trace is that a finished turn leaves something readable
// behind. Phase changes go to a live watcher and are gone.
func TestTurnLeavesATrace(t *testing.T) {
	calls := 0
	h, dir := tracedHarness(t, func(*provider.Request) (*provider.Response, error) {
		calls++
		if calls == 1 {
			return &provider.Response{
				ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Args: map[string]any{"path": "x"}}},
				Usage:     provider.Usage{PromptTokens: 100, CompletionTokens: 10},
				Model:     "test-model",
			}, nil
		}
		return &provider.Response{Content: "done", FinishReason: "stop", Model: "test-model"}, nil
	})

	if _, err := h.loop.ProcessDirectNotice(context.Background(), "read it", "cli:x", "", "", nil); err != nil {
		t.Fatal(err)
	}

	recs := readTraces(t, dir)
	if len(recs) != 1 {
		t.Fatalf("read %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Outcome != "ok" {
		t.Errorf("outcome = %q", rec.Outcome)
	}
	if rec.Trigger != "user" {
		t.Errorf("trigger = %q, want user", rec.Trigger)
	}
	if rec.Channel != "cli" || rec.Session != "cli:x" {
		t.Errorf("identity = %+v", rec)
	}
	if len(rec.Tools) != 1 || rec.Tools[0].Name != "read_file" {
		t.Errorf("tools = %+v", rec.Tools)
	}
	if rec.Tools[0].Duration < 0 {
		t.Errorf("tool duration = %v", rec.Tools[0].Duration)
	}
}

// A failed turn is the one worth being able to read afterwards.
func TestFailedTurnIsRecordedAsAnError(t *testing.T) {
	h, dir := tracedHarness(t, func(*provider.Request) (*provider.Response, error) {
		return nil, errors.New("the provider refused")
	})
	_, _ = h.loop.ProcessDirectNotice(context.Background(), "hello", "cli:x", "", "", nil)

	recs := readTraces(t, dir)
	if len(recs) != 1 {
		t.Fatalf("read %d records, want 1", len(recs))
	}
	if !recs[0].Failed() {
		t.Errorf("outcome = %q, want error", recs[0].Outcome)
	}
}

// The heartbeat is work nobody asked for, and telling it apart from an answer
// somebody is waiting on is most of what makes a trace worth reading.
func TestHeartbeatTurnIsTaggedAsSuch(t *testing.T) {
	h, dir := tracedHarness(t, func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{Content: "HEARTBEAT_OK", FinishReason: "stop", Model: "m"}, nil
	})
	if _, err := h.loop.ProcessEphemeral(context.Background(), "check"); err != nil {
		t.Fatal(err)
	}
	recs := readTraces(t, dir)
	if len(recs) != 1 || recs[0].Trigger != "heartbeat" {
		t.Fatalf("records = %+v", recs)
	}
}

// Tracing off must change nothing: the loop calls the recorder unconditionally.
func TestTurnWithoutATracerStillRuns(t *testing.T) {
	h := newHarness(t, func(*provider.Request) (*provider.Response, error) {
		return &provider.Response{Content: "fine", FinishReason: "stop", Model: "m"}, nil
	})
	h.loop.WithTracer(nil)
	reply, err := h.loop.ProcessDirectNotice(context.Background(), "hi", "cli:x", "", "", nil)
	if err != nil || reply != "fine" {
		t.Fatalf("reply = %q err = %v", reply, err)
	}
}
