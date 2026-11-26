package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

// key is a throwaway signer.
type key struct{ priv ed25519.PrivateKey }

func newKey(t *testing.T) key {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key{priv: priv}
}

func (k key) PublicKey() ed25519.PublicKey { return k.priv.Public().(ed25519.PublicKey) }
func (k key) Sign(msg []byte) []byte       { return ed25519.Sign(k.priv, msg) }
func (k key) ID() string                   { return IDOf(k.PublicKey()) }

func TestRosterChain(t *testing.T) {
	a, b, c := newKey(t), newKey(t), newKey(t)
	g := Genesis(a, NewSwarmID(), "wss://r")
	if err := g.Valid(nil); err != nil {
		t.Fatalf("genesis: %v", err)
	}
	two := g.Next(a, func(r *Roster) { r.Add(EncodeKey(b.PublicKey()), RoleMember) })
	if err := two.Valid(g); err != nil {
		t.Fatalf("epoch 2: %v", err)
	}
	if !two.Has(b.ID()) || two.Has(c.ID()) || len(two.IDs()) != 2 {
		t.Errorf("members %v", two.IDs())
	}
	// Signed by a member of the previous roster: b may sign epoch 3.
	three := two.Next(b, func(r *Roster) { r.Remove(EncodeKey(a.PublicKey())) })
	if err := three.Valid(two); err != nil {
		t.Fatalf("epoch 3: %v", err)
	}
	if three.Has(a.ID()) || !three.WasRemoved(a.ID()) {
		t.Error("removal")
	}
	// A key outside the previous roster cannot sign the next one.
	if err := g.Next(c, func(*Roster) {}).Valid(g); err == nil {
		t.Error("a stranger signed a roster")
	}
	// A roster must follow the one before it.
	if err := three.Valid(g); err == nil {
		t.Error("epoch 3 accepted on top of epoch 1")
	}
	forged := *two
	forged.Prev = "00"
	forged.Sign(a)
	if err := forged.Valid(g); err == nil {
		t.Error("a roster naming the wrong predecessor")
	}
	tampered := *two
	tampered.Home = "wss://evil"
	if err := tampered.Valid(g); err == nil {
		t.Error("a tampered roster verified")
	}
	other := g.Next(a, func(*Roster) {})
	other.Swarm = "another"
	other.Sign(a)
	if err := other.Valid(g); err == nil {
		t.Error("a roster from another swarm")
	}
	back := three.Next(b, func(r *Roster) {
		r.Members = append(r.Members, RosterMember{Key: EncodeKey(a.PublicKey()), Role: RoleMember})
	})
	if err := back.Valid(three); err == nil {
		t.Error("a removed key came back")
	}
	if err := (&Roster{Epoch: 1, Signer: EncodeKey(a.PublicKey()), Sig: "!!"}).Valid(nil); err == nil {
		t.Error("a bad signature verified")
	}
	if err := two.Valid(nil); err == nil {
		t.Error("epoch 2 passed as a genesis")
	}
	if (*Roster)(nil).Has(a.ID()) || (*Roster)(nil).IDs() != nil || (*Roster)(nil).WasRemoved(a.ID()) {
		t.Error("nil roster")
	}
	if g.Hash() == two.Hash() {
		t.Error("hashes collide")
	}
	two.Add(EncodeKey(b.PublicKey()), RoleMember)
	if len(two.Members) != 2 {
		t.Error("Add duplicated a member")
	}
}
