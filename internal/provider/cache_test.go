package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// marked reports where cache_control landed, as "system[i]" / "msg[i].block[j]"
// keys, so a test can assert placement without reaching into the wire shape.
func marked(system []anthContent, out []anthMessage) map[string]bool {
	got := map[string]bool{}
	for i, b := range system {
		if b.CacheControl != nil {
			got[key("system", i, -1)] = true
		}
	}
	for i, m := range out {
		for j, b := range m.Content {
			if b.CacheControl != nil {
				got[key("msg", i, j)] = true
			}
		}
	}
	return got
}

func key(kind string, i, j int) string {
	if j < 0 {
		return kind + strconv.Itoa(i)
	}
	return kind + strconv.Itoa(i) + ".block" + strconv.Itoa(j)
}

func TestCacheMarkOnSystemMessage(t *testing.T) {
	system, out := toAnthropic([]Message{
		{Role: "system", Content: "prompt", CacheMark: true},
		{Role: "system", Content: "summary"},
		{Role: "user", Content: "hi"},
	})
	got := marked(system, out)
	if !got["system0"] {
		t.Error("the marked system block carries no cache_control")
	}
	if got["system1"] {
		t.Error("an unmarked system block was given cache_control")
	}
}

func TestCacheMarkOnMessageLandsOnItsLastBlock(t *testing.T) {
	system, out := toAnthropic([]Message{
		{Role: "user", Content: "look", Images: []ImagePart{{MediaType: "image/png", Data: "x"}}, CacheMark: true},
	})
	got := marked(system, out)
	if !got["msg0.block1"] {
		t.Errorf("mark should ride the last block of the message, got %v", got)
	}
	if got["msg0.block0"] {
		t.Error("only the last block of a marked message is a breakpoint")
	}
}

// Consecutive tool results merge into one user message, so two marked tool
// messages have to land on two different blocks of it rather than collide.
func TestCacheMarkOnMergedToolResults(t *testing.T) {
	system, out := toAnthropic([]Message{
		{Role: "tool", ToolCallID: "a", Content: "one", CacheMark: true},
		{Role: "tool", ToolCallID: "b", Content: "two", CacheMark: true},
	})
	if len(out) != 1 {
		t.Fatalf("tool results should merge into one message, got %d", len(out))
	}
	got := marked(system, out)
	if !got["msg0.block0"] || !got["msg0.block1"] {
		t.Errorf("both marks should survive the merge, got %v", got)
	}
}

// Over the limit the request is rejected outright, so the adapter thins the
// marks itself — keeping the fixed head and the most recent tails.
func TestCacheMarksThinnedToTheLimit(t *testing.T) {
	msgs := []Message{{Role: "system", Content: "prompt", CacheMark: true}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs, Message{Role: "user", Content: "u", CacheMark: true})
		msgs = append(msgs, Message{Role: "assistant", Content: "a", CacheMark: true})
	}
	system, out := toAnthropic(msgs)
	got := marked(system, out)
	if len(got) != maxCacheBreakpoints {
		t.Fatalf("got %d breakpoints, want %d: %v", len(got), maxCacheBreakpoints, got)
	}
	if !got["system0"] {
		t.Error("thinning dropped the fixed head, which is the one every request re-reads")
	}
	last := len(out) - 1
	if !got[key("msg", last, len(out[last].Content)-1)] {
		t.Error("thinning dropped the newest tail")
	}
}

func TestNoCacheMarksMeansNoCacheControl(t *testing.T) {
	system, out := toAnthropic([]Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "hi"},
	})
	if got := marked(system, out); len(got) != 0 {
		t.Errorf("caching must stay opt-in, got %v", got)
	}
}

// The dialect reports only the uncached remainder as input, so the adapter has
// to add the cache counters back or a well-cached turn reads as almost free.
func TestAnthropicUsageFoldsCacheCountersIntoPromptTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":100,"output_tokens":7,
			"cache_creation_input_tokens":200,"cache_read_input_tokens":700}}`))
	}))
	defer srv.Close()

	p := NewAnthropic(srv.URL, "k", "claude-test")
	resp, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 1000 {
		t.Errorf("PromptTokens = %d, want 1000 (100 + 200 + 700)", resp.Usage.PromptTokens)
	}
	if resp.Usage.CacheReadTokens != 700 || resp.Usage.CacheWriteTokens != 200 {
		t.Errorf("cache counters = read %d / write %d", resp.Usage.CacheReadTokens, resp.Usage.CacheWriteTokens)
	}
	if resp.Usage.CompletionTokens != 7 {
		t.Errorf("CompletionTokens = %d", resp.Usage.CompletionTokens)
	}
}

// These dialects cache implicitly and already count the hit inside
// prompt_tokens, so the read is a subset and there is no write to report.
func TestOpenAIUsageReadsCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1000,"completion_tokens":7,
			"prompt_tokens_details":{"cached_tokens":800}}}`))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "k", "gpt-test")
	resp, err := p.Chat(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 1000 || resp.Usage.CacheReadTokens != 800 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.CacheWriteTokens != 0 {
		t.Errorf("implicit caching has no write premium to report, got %d", resp.Usage.CacheWriteTokens)
	}
}

// A cache mark describes one request. Persisting it would make the same
// history assemble differently depending on what an earlier turn happened to
// mark, which is exactly the drift the marks exist to avoid.
func TestCacheMarkIsNotSerialized(t *testing.T) {
	raw, err := json.Marshal(Message{Role: "user", Content: "hi", CacheMark: true})
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.CacheMark {
		t.Errorf("CacheMark survived a round trip through %s", raw)
	}
}

// The wire shape has to be what the API accepts: system as content blocks,
// with the marker inside the block rather than beside it.
func TestAnthropicSendsSystemAsBlocksWithCacheControl(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"content":[],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer srv.Close()

	p := NewAnthropic(srv.URL, "k", "claude-test")
	_, err := p.Chat(context.Background(), &Request{Messages: []Message{
		{Role: "system", Content: "rules", CacheMark: true},
		{Role: "user", Content: "hi"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	blocks, ok := body["system"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("system = %#v, want one content block", body["system"])
	}
	block := blocks[0].(map[string]any)
	cc, ok := block["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("cache_control = %#v", block["cache_control"])
	}
}
