package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// cameraTool returns a Result with an attached image, like screen_view does.
type cameraTool struct{ shots int }

func (c *cameraTool) Name() string        { return "camera" }
func (c *cameraTool) Description() string { return "test image tool" }
func (c *cameraTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (c *cameraTool) Execute(context.Context, map[string]any) *tools.Result {
	c.shots++
	return &tools.Result{
		ForLLM: "frame captured",
		Images: []provider.ImagePart{{MediaType: "image/png", Data: "ZnJhbWU="}},
	}
}

func countImageMessages(msgs []provider.Message) (withImages, pruned int) {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			withImages++
		}
		if strings.Contains(m.Content, "has been dropped to save space") {
			pruned++
		}
	}
	return
}

func TestToolImagesRideAUserMessage(t *testing.T) {
	h := newHarness(t,
		toolCall("camera", map[string]any{}),
		final("done"),
	)
	h.registry.Register(&cameraTool{})

	if _, err := h.loop.ProcessDirect(context.Background(), "look", "cli:img"); err != nil {
		t.Fatal(err)
	}

	// The second request must carry the image on a user message that follows
	// the tool result and says it is tool output.
	second := h.chat.requests[1].Messages
	last := second[len(second)-1]
	if last.Role != "user" || len(last.Images) != 1 {
		t.Fatalf("last message = role %s, %d images", last.Role, len(last.Images))
	}
	if !strings.Contains(last.Content, "camera") || !strings.Contains(last.Content, "not a message from the user") {
		t.Errorf("image message content = %q", last.Content)
	}
	if prev := second[len(second)-2]; prev.Role != "tool" || prev.Content != "frame captured" {
		t.Errorf("preceding message = %+v", prev)
	}

	// Persisted history never stores image bytes — only the placeholder.
	history, _ := h.store.History("cli:img")
	for _, m := range history {
		if len(m.Images) != 0 {
			t.Fatalf("image bytes persisted: %+v", m)
		}
	}
	if _, pruned := countImageMessages(history); pruned != 1 {
		t.Errorf("persisted placeholders = %d, want 1", pruned)
	}
}

func TestOldFramesArePrunedInFlight(t *testing.T) {
	h := newHarness(t,
		toolCall("camera", map[string]any{}),
		toolCall("camera", map[string]any{}),
		toolCall("camera", map[string]any{}),
		final("done"),
	)
	h.registry.Register(&cameraTool{})

	if _, err := h.loop.ProcessDirect(context.Background(), "watch", "cli:prune"); err != nil {
		t.Fatal(err)
	}

	// By the fourth request three frames were attached; only the newest
	// maxImagesInContext may still carry pixels, the rest collapse to notes.
	last := h.chat.requests[3].Messages
	withImages, pruned := countImageMessages(last)
	if withImages != maxImagesInContext {
		t.Errorf("in-flight image messages = %d, want %d", withImages, maxImagesInContext)
	}
	if pruned != 1 {
		t.Errorf("pruned notes = %d, want 1", pruned)
	}
	// The pruned note names the tool so the model knows how to refresh.
	for _, m := range last {
		if strings.Contains(m.Content, "has been dropped") && !strings.Contains(m.Content, "camera") {
			t.Errorf("pruned note lost the tool name: %q", m.Content)
		}
	}
}
