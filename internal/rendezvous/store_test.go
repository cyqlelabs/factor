package rendezvous

import (
	"path/filepath"
	"testing"

	"github.com/cyqlelabs/factor/internal/swarm"
)

func TestStoreRefusesRollbacksAndStrangers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := OpenStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := swarm.NewIdentity()
	b, _ := swarm.NewIdentity()
	g := swarm.Genesis(a, swarm.NewSwarmID(), "wss://r")
	if err := s.Commit(g); err != nil {
		t.Fatal(err)
	}
	two := g.Next(a, func(x *swarm.Roster) { x.Add(swarm.EncodeKey(b.Public), swarm.RoleMember) })
	if err := s.Commit(two); err != nil {
		t.Fatal(err)
	}
	// A second epoch-2 roster loses the race: the head is already epoch 2.
	if err := s.Commit(g.Next(a, func(*swarm.Roster) {})); err == nil {
		t.Error("a second epoch 2 was committed")
	}
	if err := s.Commit(g); err == nil {
		t.Error("a rollback was committed")
	}
	// The roster that removes b is kept for b, and changes after it are not
	// shown to it.
	c, _ := swarm.NewIdentity()
	three := two.Next(a, func(x *swarm.Roster) { x.Remove(swarm.EncodeKey(b.Public)) })
	four := three.Next(a, func(x *swarm.Roster) { x.Add(swarm.EncodeKey(c.Public), swarm.RoleMember) })
	if err := s.Commit(three); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(four); err != nil {
		t.Fatal(err)
	}
	if r := s.RemovedBy(g.Swarm, b.ID); r == nil || r.Epoch != 3 {
		t.Errorf("the roster that removed b: %+v", r)
	}
	if s.RemovedBy(g.Swarm, c.ID) != nil || s.RemovedBy("../x", b.ID) != nil {
		t.Error("a removal roster for an agent nobody removed")
	}
	// After a restart the store still holds the head.
	again, err := OpenStore(dir, false)
	if err != nil || again.Head(g.Swarm).Epoch != 4 || again.Swarms() != 1 {
		t.Fatalf("reopened: %v", err)
	}
	// A store that has gone missing refuses changes rather than starting over.
	lost, err := OpenStore(filepath.Join(t.TempDir(), "gone"), false)
	if err != nil {
		t.Fatal(err)
	}
	if lost.Refusing() == "" || lost.Commit(swarm.Genesis(a, swarm.NewSwarmID(), "")) == nil {
		t.Error("a missing store took a roster")
	}
}
