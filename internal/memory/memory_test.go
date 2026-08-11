package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
)

// fakeSmrti implements just enough of the smrti REST surface.
func fakeSmrti(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var remembered []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/remember", "/believe":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["_path"] = r.URL.Path
			body["_auth"] = r.Header.Get("Authorization")
			body["_apikey"] = r.Header.Get("X-Api-Key")
			remembered = append(remembered, body)
			_, _ = w.Write([]byte(`{"status":"ok","atom_id":"atom-1"}`))
		case "/recall":
			_, _ = w.Write([]byte(`{"memories":[
				{"id":"m1","content":"The deploy pipeline broke after force-push","type":"episode","confidence":0.9,"severity":"critical_warning","salience":0.8,"valence":-0.7},
				{"id":"m2","content":"Nico prefers Go for system tools","type":"belief","confidence":0.75,"severity":"context","salience":0.6}
			]}`))
		case "/forget":
			_, _ = w.Write([]byte(`{"status":"ok","softened":2}`))
		case "/reflect":
			_, _ = w.Write([]byte(`{"beliefs_updated":3,"atoms_pruned":1}`))
		case "/status":
			_, _ = w.Write([]byte(`{"total_atoms":42,"spaces":["main"]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &remembered
}

func TestClientRememberRoutesBeliefs(t *testing.T) {
	srv, remembered := fakeSmrti(t)
	c := NewClient(srv.URL, "smrti-key", "llm-key")

	id, err := c.Remember(context.Background(), RememberRequest{Content: "plain episode"})
	if err != nil || id != "atom-1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	_, err = c.Remember(context.Background(), RememberRequest{
		Content: "Python is best for ML", Type: "belief", Probability: 0.9, Evidence: "team survey",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(*remembered) != 2 {
		t.Fatalf("recorded %d calls", len(*remembered))
	}
	first, second := (*remembered)[0], (*remembered)[1]
	if first["_path"] != "/remember" || first["content"] != "plain episode" {
		t.Errorf("first = %v", first)
	}
	if second["_path"] != "/believe" || second["statement"] != "Python is best for ML" || second["evidence"] != "team survey" {
		t.Errorf("belief with evidence must use /believe: %v", second)
	}
	// auth split: smrti key in X-Api-Key, LLM extraction key in Authorization
	if first["_apikey"] != "smrti-key" || first["_auth"] != "Bearer llm-key" {
		t.Errorf("auth headers = %v / %v", first["_apikey"], first["_auth"])
	}
}

func TestClientRecallParsesMemories(t *testing.T) {
	srv, _ := fakeSmrti(t)
	c := NewClient(srv.URL, "", "")
	mems, err := c.Recall(context.Background(), "deploys", 5, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 || mems[0].Severity != SeverityCriticalWarning || mems[0].Valence != -0.7 {
		t.Fatalf("mems = %+v", mems)
	}
	if !c.Healthy() {
		t.Error("client should be healthy after success")
	}
}

func TestClientUnreachableMarksUnhealthy(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "", "")
	if _, err := c.Recall(context.Background(), "x", 5, 0.1); err == nil {
		t.Fatal("expected error")
	}
	if c.Healthy() {
		t.Error("client should be unhealthy after failure")
	}
}

func TestBuildRecallQuery(t *testing.T) {
	history := []provider.Message{
		{Role: "user", Content: "first message"},
		{Role: "tool", Content: "tool noise"},
		{Role: "assistant", Content: "second"},
		{Role: "user", Content: "third"},
	}
	q := BuildRecallQuery(history, "current question", 2, 500)
	if strings.Contains(q, "first") || strings.Contains(q, "tool noise") {
		t.Errorf("query kept excluded content: %q", q)
	}
	for _, want := range []string{"second", "third", "current question"} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q: %q", want, q)
		}
	}
	long := BuildRecallQuery(nil, strings.Repeat("a", 600)+"TAIL", 5, 100)
	if len(long) != 100 || !strings.HasSuffix(long, "TAIL") {
		t.Errorf("truncation must keep the tail: len=%d", len(long))
	}
}

func TestFormatMemoriesSections(t *testing.T) {
	out := FormatMemories([]Memory{
		{Content: "force-pushed to main and broke CI", Severity: SeverityCriticalWarning, Confidence: 0.92},
		{Content: "polling is fine here", Severity: SeverityKnownAntipattern, Confidence: 0.8},
		{Content: "user timezone is UTC-3", Severity: SeverityContext, Confidence: 0.7},
	}, 500)
	if !strings.Contains(out, "YOU MUST NOT repeat this past mistake (92% confidence): force-pushed") {
		t.Errorf("constraint missing: %s", out)
	}
	if !strings.Contains(out, "AVOID this known antipattern") {
		t.Errorf("antipattern missing: %s", out)
	}
	if !strings.Contains(out, "Note (70% confidence): user timezone") {
		t.Errorf("background missing: %s", out)
	}
	if FormatMemories(nil, 100) != "" {
		t.Error("no memories must produce empty prompt section")
	}
}

func TestAmbientSkipsIgnoredAndUnhealthy(t *testing.T) {
	srv, remembered := fakeSmrti(t)
	c := NewClient(srv.URL, "", "")
	_ = c.CheckHealth(context.Background())
	a := NewAmbient(c, 5, 0.3, 5, 500, 500, []string{"^HEARTBEAT_OK$"})

	a.StoreExchange("HEARTBEAT_OK", "real reply")
	if len(*remembered) != 1 || !strings.Contains((*remembered)[0]["content"].(string), "real reply") {
		t.Errorf("ignore patterns not applied: %v", *remembered)
	}

	prompt := a.MemoryPrompt(context.Background(), nil, "what about deploys?")
	if !strings.Contains(prompt, "YOU MUST NOT") {
		t.Errorf("prompt = %q", prompt)
	}

	// unhealthy engine → no injection, no panic
	dead := NewClient("http://127.0.0.1:1", "", "")
	deadAmbient := NewAmbient(dead, 5, 0.3, 5, 500, 500, nil)
	if p := deadAmbient.MemoryPrompt(context.Background(), nil, "q"); p != "" {
		t.Errorf("unhealthy engine injected: %q", p)
	}
}

func TestDeriveExtract(t *testing.T) {
	prov := config.ProviderConfig{Type: "openrouter", APIKey: "sk-or", Model: "meta/llama"}
	es := DeriveExtract(config.MemoryConfig{}, prov)
	if es.Mode != "hybrid" || es.URL != "https://openrouter.ai/api" || es.Key != "sk-or" || es.Model != "meta/llama" {
		t.Errorf("derived = %+v", es)
	}

	es = DeriveExtract(config.MemoryConfig{ExtractMode: "local"}, prov)
	if es.Mode != "local" || es.URL != "" {
		t.Errorf("local = %+v", es)
	}

	anthOnly := config.ProviderConfig{Type: "anthropic", APIKey: "ak", Model: "claude"}
	es = DeriveExtract(config.MemoryConfig{}, anthOnly)
	if es.Mode != "local" {
		t.Errorf("anthropic-only should fall back to local extraction: %+v", es)
	}

	es = DeriveExtract(config.MemoryConfig{ExtractURL: "http://127.0.0.1:11434/v1", ExtractModel: "qwen3"}, prov)
	if es.URL != "http://127.0.0.1:11434" || es.Model != "qwen3" {
		t.Errorf("explicit = %+v", es)
	}
}

func TestSidecarAdoptsRunningServer(t *testing.T) {
	srv, _ := fakeSmrti(t)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	cfg := config.Default().Memory
	cfg.Host, cfg.Port = u.Hostname(), port
	cfg.Command = "/nonexistent-binary-that-must-not-run"

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	deadline := time.Now().Add(3 * time.Second)
	for !eng.Healthy() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !eng.Healthy() {
		t.Fatal("engine did not adopt the running server")
	}
	if _, err := eng.Recall(context.Background(), "anything", 3, 0.1); err != nil {
		t.Fatal(err)
	}
}

func TestSidecarDegradedWhenBinaryMissing(t *testing.T) {
	cfg := config.Default().Memory
	cfg.Host, cfg.Port = "127.0.0.1", 1 // nothing listens here
	cfg.Command = "/nonexistent-binary-that-must-not-run"
	cfg.StartupTimeoutSecs = 1

	eng, err := NewEngine(context.Background(), cfg, ExtractSettings{Mode: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if eng.Healthy() {
		t.Error("engine cannot be healthy without a server")
	}
	done := make(chan struct{})
	go func() { _ = eng.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
}

func TestNewEngineModes(t *testing.T) {
	eng, err := NewEngine(context.Background(), config.MemoryConfig{Mode: "off"}, ExtractSettings{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := eng.(Noop); !ok {
		t.Errorf("off mode = %T", eng)
	}
	if _, err := NewEngine(context.Background(), config.MemoryConfig{Mode: "bogus"}, ExtractSettings{}, ""); err == nil {
		t.Error("bogus mode accepted")
	}
}
