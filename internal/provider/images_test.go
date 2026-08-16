package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureServer records the request body and replies with a minimal valid
// response for the given wire dialect.
func captureServer(t *testing.T, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	captured := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func imageMessages() []Message {
	return []Message{
		{Role: "user", Content: "hi"},
		{Role: "user", Content: "look at this",
			Images: []ImagePart{{MediaType: "image/png", Data: "aGVsbG8="}}},
	}
}

func TestOpenAIImagesBecomeContentParts(t *testing.T) {
	srv, captured := captureServer(t, `{
		"choices":[{"message":{"role":"assistant","content":"seen"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`)

	p := NewOpenAI(srv.URL, "k", "m")
	resp, err := p.Chat(context.Background(), &Request{Messages: imageMessages()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "seen" {
		t.Errorf("content = %q", resp.Content)
	}

	msgs := (*captured)["messages"].([]any)
	// A plain user message stays a bare string — the cache-friendly form
	// every endpoint accepts.
	if plain := msgs[0].(map[string]any); plain["content"] != "hi" {
		t.Errorf("plain content = %v (%T), want string", plain["content"], plain["content"])
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content parts = %v", parts)
	}
	text := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "look at this" {
		t.Errorf("text part = %v", text)
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("image part = %v", img)
	}
	if url := img["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,aGVsbG8=" {
		t.Errorf("image url = %v", url)
	}
}

func TestAnthropicImagesBecomeSourceBlocks(t *testing.T) {
	srv, captured := captureServer(t, `{
		"content":[{"type":"text","text":"seen"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1}}`)

	p := NewAnthropic(srv.URL, "k", "m")
	resp, err := p.Chat(context.Background(), &Request{Messages: imageMessages()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "seen" {
		t.Errorf("content = %q", resp.Content)
	}

	msgs := (*captured)["messages"].([]any)
	blocks := msgs[1].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v", blocks)
	}
	img := blocks[1].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("image block = %v", img)
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "aGVsbG8=" {
		t.Errorf("source = %v", src)
	}
	// The imageless message keeps a single text block.
	if plain := msgs[0].(map[string]any)["content"].([]any); len(plain) != 1 {
		t.Errorf("plain user blocks = %v", plain)
	}
}
