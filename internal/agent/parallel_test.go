package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// blockingTool records how many of its calls were in flight at once, which is
// the only honest way to tell concurrency from a fast sequence.
type blockingTool struct {
	name     string
	parallel bool

	inChan  chan struct{}
	release chan struct{}

	live    atomic.Int32
	highest atomic.Int32
	runs    atomic.Int32
}

func newBlockingTool(name string, parallel bool) *blockingTool {
	return &blockingTool{
		name:     name,
		parallel: parallel,
		inChan:   make(chan struct{}, 16),
		release:  make(chan struct{}),
	}
}

func (b *blockingTool) Name() string        { return b.name }
func (b *blockingTool) Description() string { return "test tool" }
func (b *blockingTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"n": map[string]any{"type": "string"},
	}}
}

func (b *blockingTool) ParallelSafe(map[string]any) bool { return b.parallel }

func (b *blockingTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	n := b.live.Add(1)
	for {
		high := b.highest.Load()
		if n <= high || b.highest.CompareAndSwap(high, n) {
			break
		}
	}
	b.runs.Add(1)
	b.inChan <- struct{}{}
	<-b.release
	b.live.Add(-1)
	return tools.Text("done " + tools.StringArg(args, "n"))
}

// waitFor blocks until n calls have entered Execute, then lets them all go.
func (b *blockingTool) waitFor(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-b.inChan:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of %d calls entered the tool", i, n)
		}
	}
	close(b.release)
}

func callsTo(name string, args ...string) []provider.ToolCall {
	var out []provider.ToolCall
	for i, a := range args {
		out = append(out, provider.ToolCall{
			ID:   name + string(rune('a'+i)),
			Name: name,
			Args: map[string]any{"n": a},
		})
	}
	return out
}

// A batch of declared-safe calls runs at once. Without this the batch costs
// the sum of its round trips; with it, the longest.
func TestParallelSafeBatchRunsConcurrently(t *testing.T) {
	h := newHarness(t)
	tool := newBlockingTool("reader", true)
	h.loop.registry.Register(tool)

	calls := callsTo("reader", "1", "2", "3")
	done := make(chan []toolOutcome, 1)
	go func() { done <- h.loop.runTools(context.Background(), "cli:x", calls) }()

	tool.waitFor(t, 3)
	outcomes := <-done

	if got := tool.highest.Load(); got != 3 {
		t.Errorf("peak concurrency = %d, want 3", got)
	}
	for i, want := range []string{"done 1", "done 2", "done 3"} {
		if outcomes[i].content != want {
			t.Errorf("outcome %d = %q, want %q", i, outcomes[i].content, want)
		}
	}
}

// One undeclared call makes the whole batch sequential: the guarantee is about
// the batch, not the tool. A read running beside a click is still a race.
func TestOneUnsafeCallMakesTheBatchSequential(t *testing.T) {
	h := newHarness(t)
	safe := newBlockingTool("reader", true)
	unsafe := newBlockingTool("clicker", false)
	close(safe.release)
	close(unsafe.release)
	h.loop.registry.Register(safe, unsafe)

	calls := append(callsTo("reader", "1", "2"), callsTo("clicker", "3")...)
	h.loop.runTools(context.Background(), "cli:x", calls)

	if got := safe.highest.Load(); got > 1 {
		t.Errorf("peak concurrency = %d, want 1: an unsafe call must serialize its batch", got)
	}
}

func TestSingleCallIsNeverParallel(t *testing.T) {
	h := newHarness(t)
	tool := newBlockingTool("reader", true)
	close(tool.release)
	h.loop.registry.Register(tool)

	h.loop.runTools(context.Background(), "cli:x", callsTo("reader", "1"))
	if got := tool.highest.Load(); got != 1 {
		t.Errorf("peak concurrency = %d, want 1", got)
	}
}

// The dedup that landed before this rework has to survive it: the same call
// twice in one batch is answered once, and both ids still get a result.
func TestRunToolsAnswersRepeatedCallsOnce(t *testing.T) {
	h := newHarness(t)
	tool := newBlockingTool("reader", true)
	close(tool.release)
	h.loop.registry.Register(tool)

	calls := []provider.ToolCall{
		{ID: "a", Name: "reader", Args: map[string]any{"n": "same"}},
		{ID: "b", Name: "reader", Args: map[string]any{"n": "same"}},
	}
	outcomes := h.loop.runTools(context.Background(), "cli:x", calls)

	if got := tool.runs.Load(); got != 1 {
		t.Errorf("the tool ran %d times, want 1", got)
	}
	if outcomes[0].content != outcomes[1].content || outcomes[0].content == "" {
		t.Errorf("both ids need the same answer, got %q and %q", outcomes[0].content, outcomes[1].content)
	}
}

// A repeat must not bring a second copy of a picture into the context.
func TestRepeatedCallDoesNotDuplicateImages(t *testing.T) {
	h := newHarness(t)
	h.loop.registry.Register(&imageTool{})

	calls := []provider.ToolCall{
		{ID: "a", Name: "shot", Args: map[string]any{}},
		{ID: "b", Name: "shot", Args: map[string]any{}},
	}
	outcomes := h.loop.runTools(context.Background(), "cli:x", calls)
	if len(outcomes[0].images) != 1 {
		t.Fatalf("first call lost its image: %+v", outcomes[0])
	}
	if len(outcomes[1].images) != 0 {
		t.Errorf("the repeat carried a second frame: %+v", outcomes[1])
	}
}

type imageTool struct{ tools.ReadOnly }

func (imageTool) Name() string               { return "shot" }
func (imageTool) Description() string        { return "test" }
func (imageTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (imageTool) Execute(context.Context, map[string]any) *tools.Result {
	return &tools.Result{
		ForLLM: "a screen",
		Images: []provider.ImagePart{{MediaType: "image/png", Data: "x"}},
	}
}

// A cancelled turn still needs a result beside every id, and that result must
// not read as the tool having failed.
func TestCancelledBatchReportsInterruptionNotFailure(t *testing.T) {
	h := newHarness(t)
	tool := newBlockingTool("reader", true)
	close(tool.release)
	h.loop.registry.Register(tool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcomes := h.loop.runTools(ctx, "cli:x", callsTo("reader", "1", "2"))

	for i, o := range outcomes {
		if o.content != interruptedTool {
			t.Errorf("outcome %d = %q, want the interruption note", i, o.content)
		}
	}
	if got := tool.runs.Load(); got != 0 {
		t.Errorf("a cancelled batch ran %d tools", got)
	}
}
