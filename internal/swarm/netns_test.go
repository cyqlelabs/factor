//go:build netns

package swarm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
)

// TestNetnsAgent is one agent of the network-namespace checkpoint, run by
// test/netns/run.sh: each agent is its own process in its own namespace
// behind its own NAT, and the agents coordinate through files in a directory
// they share. desk and lap share one home network; home sits on another that
// uses the same private addresses, so the address home publishes for itself
// belongs to lap on desk's network.
//
// desk invites home, home invites lap, desk learns about lap from the roster,
// desk times calls to both, and then desk removes lap while home and lap
// watch.
func TestNetnsAgent(t *testing.T) {
	role, dir := os.Getenv("NETNS_ROLE"), os.Getenv("NETNS_DIR")
	if role == "" || dir == "" {
		t.Skip("run by test/netns/run.sh")
	}
	svc, err := New(config.SwarmConfig{Enabled: true, Name: role, Rendezvous: os.Getenv("NETNS_RENDEZVOUS"),
		RemoteTasks: 2, RemoteQueue: 2, LAN: true},
		t.TempDir(), "netns", replyTurns{reply: func(rt agent.RemoteTurn) string { return role + " saw: " + rt.Content }}, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	svc.Connect()
	defer svc.Close()
	n := &netnsRun{t: t, dir: dir, svc: svc}
	switch role {
	case "desk":
		n.desk()
	case "home":
		n.home()
	case "lap":
		n.lap()
	default:
		t.Fatalf("unknown role %q", role)
	}
}

type netnsRun struct {
	t   *testing.T
	dir string
	svc *Service
}

func (n *netnsRun) put(name, content string) {
	n.t.Helper()
	tmp := filepath.Join(n.dir, "."+name)
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		n.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(n.dir, name)); err != nil {
		n.t.Fatal(err)
	}
}

func (n *netnsRun) await(name string) string {
	n.t.Helper()
	var got string
	n.until("file "+name, 90*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(n.dir, name))
		got = string(data)
		return err == nil
	})
	return got
}

func (n *netnsRun) until(what string, d time.Duration, ok func() bool) {
	n.t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			n.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (n *netnsRun) invite(peer string) {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	done := make(chan error, 1)
	if err := n.svc.Invite(ctx, "", func(code string) error { n.put("code-"+peer, code); return nil },
		func(_ string, err error) { done <- err }); err != nil {
		n.t.Fatalf("invite %s: %v", peer, err)
	}
	if err := <-done; err != nil {
		n.t.Fatalf("invite %s: %v", peer, err)
	}
}

// join enters the code the inviter wrote and reports how long pairing took
// from the moment the code was entered.
func (n *netnsRun) join(self string) {
	n.t.Helper()
	code := n.await("code-" + self)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := n.svc.Join(ctx, code, ""); err != nil {
		n.t.Fatalf("join: %v", err)
	}
	n.put("pairing-"+self, time.Since(start).String())
	n.put("id-"+self, n.svc.ID())
}

func (n *netnsRun) direct(peer string) string {
	lan := n.svc.lanState()
	if lan == nil {
		return ""
	}
	lan.mu.Lock()
	defer lan.mu.Unlock()
	return lan.good[peer]
}

// timing is one member's calls: the first after the tunnel's connections were
// closed, then warm ones.
type timing struct {
	Path   string   `json:"path"`
	Cold   string   `json:"cold"`
	Warm   []string `json:"warm"`
	Median string   `json:"median"`
}

func (n *netnsRun) timeCalls(name, peer string) timing {
	n.t.Helper()
	call := func(msg string) time.Duration {
		start := time.Now()
		res := relaySend(n.t, n.svc, name, msg)
		took := time.Since(start)
		if res.IsError || !strings.Contains(res.ForLLM, name+" saw: "+msg) {
			n.t.Fatalf("call to %s: %q", name, res.ForLLM)
		}
		return took
	}
	_, _, tunnel := n.svc.net()
	tunnel.closeIdle()
	out := timing{Path: "relay", Cold: call("cold").String()}
	if addr := n.direct(peer); addr != "" {
		out.Path = "direct " + addr
	}
	var warm []time.Duration
	for i := range 20 {
		warm = append(warm, call("warm "+strconv.Itoa(i)))
	}
	for _, d := range warm {
		out.Warm = append(out.Warm, d.String())
	}
	slices.Sort(warm)
	out.Median = warm[len(warm)/2].String()
	return out
}

func (n *netnsRun) desk() {
	n.invite("home")
	homeID := n.await("id-home")
	lapID := n.await("id-lap")
	n.until("desk to learn about lap from the roster", 30*time.Second, func() bool { return n.svc.IsMember(lapID) })
	n.until("names", 30*time.Second, func() bool {
		st := n.svc.Book().Read()
		return st.Peer(homeID).Name == "home" && st.Peer(lapID).Name == "lap"
	})
	n.until("a direct path to lap", 30*time.Second, func() bool { return n.direct(lapID) != "" })
	report := map[string]any{
		"pairing_home": n.await("pairing-home"),
		"pairing_lap":  n.await("pairing-lap"),
		"home":         n.timeCalls("home", homeID),
		"lap":          n.timeCalls("lap", lapID),
	}
	// home published lap's address on this network: the pinned handshake must
	// have refused it rather than taken lap for home.
	if addr := n.direct(homeID); addr != "" {
		n.t.Errorf("a direct path to home at %s, which is lap's address here", addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := n.svc.Remove(ctx, lapID); err != nil {
		n.t.Fatalf("remove lap: %v", err)
	}
	n.await("home-saw-removal")
	n.await("lap-removed")
	report["removal_seen_by_both"] = time.Since(start).String()
	data, _ := json.MarshalIndent(report, "", "  ")
	n.put("result.json", string(data))
	n.put("stop", "")
}

func (n *netnsRun) home() {
	n.join("home")
	n.invite("lap")
	lapID := n.await("id-lap")
	n.until("home to see lap on the roster", 30*time.Second, func() bool { return n.svc.IsMember(lapID) })
	n.until("home to see lap removed", 60*time.Second, func() bool { return !n.svc.IsMember(lapID) })
	n.put("home-saw-removal", "")
	n.await("stop")
}

func (n *netnsRun) lap() {
	n.join("lap")
	n.until("lap to see itself removed", 90*time.Second, func() bool { return !n.svc.Book().Read().Joined() })
	n.put("lap-removed", "")
	n.await("stop")
}
