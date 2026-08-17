package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIdleTracksTheGraphAndNotTheProbes(t *testing.T) {
	srv, _ := fakeSmrti(t)
	c := NewClient(srv.URL, "", "")

	if !c.Idle(time.Hour) {
		t.Fatal("a client that has touched nothing is idle")
	}

	// The supervisor probes health every 30 seconds. If that counted, the
	// engine would never look idle and could never be upgraded.
	if err := c.CheckHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.Idle(time.Hour) {
		t.Fatal("a status probe is not graph activity")
	}

	if _, err := c.Remember(context.Background(), RememberRequest{Content: "something worth keeping"}); err != nil {
		t.Fatal(err)
	}
	if c.Idle(time.Hour) {
		t.Error("a store that just landed is recent activity")
	}
	if !c.Idle(0) {
		t.Error("with nothing in flight, an elapsed quiet window is quiet")
	}

	if _, err := c.Recall(context.Background(), "what happened", 1, 0, Scope{}); err != nil {
		t.Fatal(err)
	}
	if c.Idle(time.Hour) {
		t.Error("a recall is graph activity too")
	}
	if err := c.Forget(context.Background(), "something", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reflect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Idle(time.Hour) {
		t.Error("reflection rewrites the graph")
	}
}

func TestClientIsBusyWhileACallIsInFlight(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(arrived)
		<-release
		_, _ = w.Write([]byte(`{"memories":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Recall(context.Background(), "what happened", 1, 0, Scope{})
	}()

	<-arrived
	if c.Idle(0) {
		t.Error("a call still on the wire is not an idle graph")
	}
	close(release)
	<-done
	if !c.Idle(0) {
		t.Error("the call finished; the graph is quiet again")
	}
}

func TestIdleFunc(t *testing.T) {
	if !IdleFunc(Noop{}, time.Hour)() {
		t.Error("an engine with no graph of its own has nothing to interrupt")
	}

	srv, _ := fakeSmrti(t)
	s := &Sidecar{client: NewClient(srv.URL, "", ""), external: true}
	idle := IdleFunc(s, time.Hour)
	if !idle() {
		t.Fatal("an untouched engine is idle")
	}
	if _, err := s.Remember(context.Background(), RememberRequest{Content: "keep this"}); err != nil {
		t.Fatal(err)
	}
	if idle() {
		t.Error("the sidecar must report what its client just did")
	}
}
