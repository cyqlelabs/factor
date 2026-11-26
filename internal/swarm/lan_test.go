package swarm

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
)

func init() { lanPort = 0 }

// lanAgent is an agent with the direct path on, published at loopback.
func (r *relay) lanAgent(name string) *Service {
	r.t.Helper()
	svc, err := New(config.SwarmConfig{Enabled: true, Name: name, Rendezvous: r.url, RemoteTasks: 2, LAN: true},
		r.t.TempDir(), "test", replyTurns{reply: func(rt agent.RemoteTurn) string { return name + " heard " + rt.Content }}, bus.New())
	if err != nil {
		r.t.Fatal(err)
	}
	svc.lanAddrs = func(port int) []string {
		return []string{"127.0.0.1:1", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	}
	svc.Connect()
	r.t.Cleanup(svc.Close)
	return svc
}

func TestDirectPathOnOneNetwork(t *testing.T) {
	r := newRelay(t)
	desk, home := r.lanAgent("desk"), r.lanAgent("home")
	code, done := invite(t, desk)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := home.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	<-done
	// The first call goes over the relay while the probe runs beside it.
	start := time.Now()
	if res := relaySend(t, desk, "home", "first"); !strings.Contains(res.ForLLM, "home heard first") {
		t.Fatalf("relayed: %q", res.ForLLM)
	}
	t.Logf("first call, relayed: %s", time.Since(start))
	waitFor(t, "a direct path", func() bool { return desk.directCount() == 1 && home.directCount() == 1 })
	if !strings.Contains(desk.Status(), "1 direct") {
		t.Errorf("status %q", desk.Status())
	}
	start = time.Now()
	if res := relaySend(t, desk, "home", "direct"); !strings.Contains(res.ForLLM, "home heard direct") {
		t.Fatalf("direct: %q", res.ForLLM)
	}
	t.Logf("call over the direct path: %s", time.Since(start))
	// With the rendezvous gone, the direct path still carries the work.
	r.hs.CloseClientConnections()
	r.hs.Close()
	if res := relaySend(t, desk, "home", "alone"); !strings.Contains(res.ForLLM, "home heard alone") {
		t.Fatalf("without the rendezvous: %q", res.ForLLM)
	}
	if got := home.Book().Read().Peer(desk.ID()).Addrs; len(got) != 2 {
		t.Errorf("addresses kept: %v", got)
	}
}

func TestDirectPathRules(t *testing.T) {
	n := newNode(t, "desk")
	if _, err := n.svc.dialDirect(context.Background(), "x"); err == nil {
		t.Error("dialled directly with the path off")
	}
	n.svc.discoverAll()
	n.svc.discover("x")
	if n.svc.directCount() != 0 {
		t.Error("direct count with the path off")
	}
	lan := &lanPath{good: map[string]string{"x": "127.0.0.1:1"}}
	n.svc.lan = lan
	if _, err := n.svc.dialDirect(context.Background(), "x"); err == nil || lan.good["x"] != "" {
		t.Error("a dead direct address was kept")
	}
	if n.svc.probe(context.Background(), "x", "127.0.0.1:1") {
		t.Error("a closed port probed as a path")
	}
	for _, a := range localAddrs(9) {
		if strings.HasPrefix(a, "127.") {
			t.Errorf("loopback published: %s", a)
		}
	}
	if boolKey(true) != "1" || boolKey(false) != "0" {
		t.Error("boolKey")
	}
}
