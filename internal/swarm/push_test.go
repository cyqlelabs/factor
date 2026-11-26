package swarm

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
)

func TestPushWakesTheFollower(t *testing.T) {
	r := newRelay(t)
	release := make(chan struct{})
	home := r.agent("home", func(agent.RemoteTurn) string { <-release; return "built it" })
	dir := t.TempDir()
	deskBus := bus.New()
	cfg := config.SwarmConfig{Enabled: true, Name: "desk", Rendezvous: r.url, RemoteTasks: 1}
	desk, err := New(cfg, dir, "test", replyTurns{reply: func(agent.RemoteTurn) string { return "" }}, deskBus)
	if err != nil {
		t.Fatal(err)
	}
	desk.Connect()
	code, done := invite(t, home)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := desk.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	<-done
	if res := relaySend(t, desk, home.ID(), "build the report"); !strings.Contains(res.ForLLM, "Passed to") {
		t.Fatalf("send: %q", res.ForLLM)
	}
	// The caller restarts; from now on it would only poll once an hour.
	desk.Close()
	again, err := New(cfg, dir, "test", replyTurns{reply: func(agent.RemoteTurn) string { return "" }}, deskBus)
	if err != nil {
		t.Fatal(err)
	}
	again.pollEvery = time.Hour
	t.Cleanup(again.Close)
	again.Connect()
	waitFor(t, "the restarted caller to reconnect", func() bool { _, l, _ := again.net(); return l.Online() })
	again.Start()
	close(release)
	select {
	case msg := <-deskBus.Inbound():
		if !strings.Contains(msg.Content, "built it") {
			t.Errorf("delivered %q", msg.Content)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the push never woke the follower")
	}
}

func TestPushRefusalsAndReceiver(t *testing.T) {
	desk, home := newNode(t, "desk"), newNode(t, "home")
	pair(t, map[string]*node{"desk": desk, "home": home})
	p := tunnelPusher{s: home.svc}
	ev := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t", ContextID: "c"}, a2a.TaskStateWorking, nil)
	for _, u := range []string{
		"http://" + desk.svc.ID() + ".swarm/swarm/v1/push",
		"https://evil.example/swarm/v1/push",
		"https://stranger.swarm/swarm/v1/push",
		"https://" + desk.svc.ID() + ".swarm/elsewhere",
		"::",
	} {
		if err := p.send(context.Background(), &a2a.PushConfig{URL: u}, ev); err == nil {
			t.Errorf("pushed to %s", u)
		}
	}
	if err := p.SendPush(context.Background(), &a2a.PushConfig{URL: "::"}, ev); err != nil {
		t.Error("a failed push failed the task")
	}
	good := desk.svc.pushConfig(&Delegation{Token: "tok"})
	if err := p.send(context.Background(), good, ev); err != nil {
		t.Errorf("push to a member: %v", err)
	}

	h := desk.svc.Handler()
	post := func(peer, body, token string) int {
		req := httptest.NewRequest("POST", "/"+pushPath, bytes.NewBufferString(body))
		if peer != "" {
			req.Header.Set("X-Test-Peer", peer)
		}
		req.Header.Set(pushHeader, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if post("", "{}", "") != http.StatusUnauthorized {
		t.Error("an unknown agent pushed")
	}
	if post(home.svc.ID(), "junk", "") != http.StatusBadRequest {
		t.Error("a junk push was taken")
	}
	ch, release := desk.svc.follower("d1")
	defer release()
	desk.svc.deleg.save(&Delegation{ID: "d1", Peer: home.svc.ID(), Task: "t", Token: "tok", Updated: time.Now()})
	body := `{"statusUpdate":{"taskId":"t","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}}`
	if post(home.svc.ID(), body, "wrong") != http.StatusNoContent {
		t.Error("push status")
	}
	select {
	case <-ch:
		t.Error("a push with the wrong token woke the follower")
	default:
	}
	post(home.svc.ID(), body, "tok")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Error("the push did not wake the follower")
	}
	if join("http://x/", "/y") != "http://x/y" || join("http://x", "y") != "http://x/y" {
		t.Error("join")
	}
}
