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
	// After a restart the store still holds the head.
	again, err := OpenStore(dir, false)
	if err != nil || again.Head(g.Swarm).Epoch != 2 || again.Swarms() != 1 {
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
