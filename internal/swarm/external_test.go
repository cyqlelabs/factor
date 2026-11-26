package swarm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	fa2a "github.com/cyqlelabs/factor/internal/a2a"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

type outsideRunner struct{}

func (outsideRunner) Run(_ context.Context, t fa2a.Turn) (string, error) {
	if t.HasMeta {
		return "", nil
	}
	return "outside: " + t.Text, nil
}

// outsideAgent is somebody else's A2A server, which wants a bearer token.
func outsideAgent(t *testing.T) string {
	t.Helper()
	store, err := fa2a.OpenStore(filepath.Join(t.TempDir(), "tasks"), fa2a.KeepTasks)
	if err != nil {
		t.Fatal(err)
	}
	srv := fa2a.NewServer(outsideRunner{}, store, fa2a.Options{})
	hs := httptest.NewServer(srv.Handler(func(r *http.Request) (string, bool) {
		return "factor", r.Header.Get("Authorization") == "Bearer s3cret"
	}))
	t.Cleanup(func() { hs.Close(); srv.Close() })
	return hs.URL
}

func TestAgentsOutsideTheSwarm(t *testing.T) {
	url := outsideAgent(t)
	cfg := config.SwarmConfig{Enabled: true, Name: "desk", RemoteTasks: 1, Agents: map[string]config.ExternalAgent{
		"helper":   {URL: url, Token: "s3cret"},
		"intimate": {URL: url, Token: "s3cret", Private: true},
		"plain":    {URL: "http://example.com/a2a"},
	}}
	svc, err := New(cfg, t.TempDir(), "test", &fakeTurns{}, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	send := func(tc tools.ToolContext, who string) *tools.Result {
		return svc.Tools()[0].Execute(tools.WithToolContext(context.Background(), tc), map[string]any{"agent": who, "message": "hi"})
	}
	private := telegram
	shared := telegram
	shared.Audience = tools.AudienceShared

	if res := send(private, "helper"); !res.IsError || !strings.Contains(res.ForLLM, "private") {
		t.Errorf("a private conversation reached an agent not granted it: %q", res.ForLLM)
	}
	if res := send(shared, "helper"); res.IsError || !strings.Contains(res.ForLLM, "outside: hi") {
		t.Errorf("in company: %q", res.ForLLM)
	}
	if res := send(private, "intimate"); res.IsError || !strings.Contains(res.ForLLM, "outside: hi") {
		t.Errorf("granted private: %q", res.ForLLM)
	}
	remote := tools.ToolContext{Channel: "a2a", ChatID: "x", SessionKey: "a2a:x", Trigger: "a2a", Audience: tools.AudienceShared}
	if res := send(remote, "intimate"); !res.IsError {
		t.Error("a remote turn reached an agent outside the swarm")
	}
	job := shared
	job.Trigger = "job"
	if res := send(job, "intimate"); !res.IsError {
		t.Error("a job reached an agent outside the swarm")
	}
	if res := send(shared, "plain"); !strings.Contains(res.ForLLM, "https") {
		t.Errorf("plain http to a remote host: %q", res.ForLLM)
	}
	if res := send(shared, "nobody"); !res.IsError {
		t.Error("sent to an agent nobody configured")
	}
	if _, _, err := svc.clientFor(externalID("ghost")); err == nil {
		t.Error("a client for an unconfigured agent")
	}
	if list := strings.Join(svc.externalNames(), "\n"); !strings.Contains(list, "helper") || !strings.Contains(list, "private conversations too") {
		t.Errorf("external list %q", list)
	}
	if (State{}).Label(externalID("helper")) != "helper" {
		t.Error("label of an outside agent")
	}
}
