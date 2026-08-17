package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const (
	statusWithSpaces    = `{"total_atoms":1,"by_type":{},"personality":{},"spaces":["main"],"version":"0.9.0"}`
	statusWithoutSpaces = `{"total_atoms":1,"by_type":{},"personality":{}}`
)

// spaceCapture records the JSON body of every POST and serves a swappable /status.
type spaceCapture struct {
	mu     sync.Mutex
	bodies map[string]map[string]any
	status string
}

func (c *spaceCapture) setStatus(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
}

func (c *spaceCapture) body(path string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[path]
}

func newSpaceCaptureServer(t *testing.T, statusJSON string) (*httptest.Server, *spaceCapture) {
	t.Helper()
	c := &spaceCapture{bodies: map[string]map[string]any{}, status: statusJSON}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.URL.Path == "/status" {
			_, _ = w.Write([]byte(c.status))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.bodies[r.URL.Path] = body
		if r.URL.Path == "/recall" {
			_, _ = w.Write([]byte(`{"memories":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","atom_id":"a1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestSpaceFieldsOmittedUntilStatusProvesSupport(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithoutSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()

	// Before any status probe the engine's capabilities are unknown: an old
	// engine would silently misroute a space field into its default space, so
	// nothing may be sent.
	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; ok {
		t.Error("space sent before any status probe")
	}

	// A status without a spaces key is an old engine: still nothing.
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; ok {
		t.Error("space sent to an engine that never advertised spaces")
	}

	if _, err := c.Recall(ctx, "q", 5, 0.1, Scope{Space: "system", ReadSpaces: []string{"system", "main"}}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"space", "read_spaces"} {
		if _, ok := cap.body("/recall")[k]; ok {
			t.Errorf("%s sent to an engine that never advertised spaces", k)
		}
	}

	if err := c.Forget(ctx, "q", "", "system"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/forget")["space"]; ok {
		t.Error("space sent to an engine that never advertised spaces")
	}
}

func TestSpaceFieldsSentOnceStatusAdvertisesSpaces(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/remember")["space"]; got != "system" {
		t.Errorf("remember space = %v, want system", got)
	}

	if _, err := c.Remember(ctx, RememberRequest{Content: "b", Type: "belief", Evidence: "e", Probability: 0.9, Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/believe")["space"]; got != "system" {
		t.Errorf("believe space = %v, want system", got)
	}

	if _, err := c.Recall(ctx, "q", 5, 0.1, Scope{Space: "system", ReadSpaces: []string{"system", "main"}}); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/recall")["space"]; got != "system" {
		t.Errorf("recall space = %v, want system", got)
	}
	rs, _ := cap.body("/recall")["read_spaces"].([]any)
	if len(rs) != 2 || rs[0] != "system" || rs[1] != "main" {
		t.Errorf("recall read_spaces = %v, want [system main]", rs)
	}

	if err := c.Forget(ctx, "q", "why", "system"); err != nil {
		t.Fatal(err)
	}
	if got := cap.body("/forget")["space"]; got != "system" {
		t.Errorf("forget space = %v, want system", got)
	}
}

func TestZeroScopeStaysByteIdenticalEvenWhenCapable(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Remember(ctx, RememberRequest{Content: "x"}); err != nil {
		t.Fatal(err)
	}
	for k := range cap.body("/remember") {
		if k != "content" && k != "type" && k != "probability" {
			t.Errorf("unexpected remember field %q with a zero scope", k)
		}
	}

	if _, err := c.Recall(ctx, "q", 5, 0.1, Scope{}); err != nil {
		t.Fatal(err)
	}
	for k := range cap.body("/recall") {
		if k != "query" && k != "top_k" && k != "min_confidence" {
			t.Errorf("unexpected recall field %q with a zero scope", k)
		}
	}
}

func TestCapabilityFollowsTheLatestStatus(t *testing.T) {
	srv, cap := newSpaceCaptureServer(t, statusWithSpaces)
	c := NewClient(srv.URL, "", "")
	ctx := context.Background()
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Content: "x", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; !ok {
		t.Fatal("space not sent to a capable engine")
	}

	// The sidecar can be replaced by an older engine between probes; the next
	// status must withdraw the capability.
	cap.setStatus(statusWithoutSpaces)
	if err := c.CheckHealth(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Content: "y", Space: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.body("/remember")["space"]; ok {
		t.Error("space still sent after the engine stopped advertising spaces")
	}
}
