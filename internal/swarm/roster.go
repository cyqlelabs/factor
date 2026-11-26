package swarm

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Roles a member can hold. Guest exists from the start so a swarm two people
// share needs no new format.
const (
	RoleMember = "member"
	RoleGuest  = "guest"
)

// RosterMember is one key in the roster.
type RosterMember struct {
	Key  string `json:"key"`
	Role string `json:"role"`
}

// Roster says who belongs to a swarm, and nothing else: names and grants are
// each agent's own business and never reach the rendezvous.
type Roster struct {
	Swarm   string         `json:"swarm"`
	Epoch   int            `json:"epoch"`
	Prev    string         `json:"prev,omitempty"`
	Home    string         `json:"home"`
	Members []RosterMember `json:"members"`
	Removed []string       `json:"removed,omitempty"`
	Signer  string         `json:"signer"`
	Sig     string         `json:"sig,omitempty"`
}

// NewSwarmID makes the random id a new swarm is known by.
func NewSwarmID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return strings.ToLower(idEncoding.EncodeToString(b))
}

// Genesis is the first roster of a new swarm: this agent alone.
func Genesis(id *Identity, swarm, home string) *Roster {
	r := &Roster{Swarm: swarm, Epoch: 1, Home: home,
		Members: []RosterMember{{Key: EncodeKey(id.Public), Role: RoleMember}}}
	r.Sign(id)
	return r
}

func (r *Roster) canonical(withSig bool) []byte {
	c := *r
	c.Members = slices.Clone(r.Members)
	sort.Slice(c.Members, func(i, j int) bool { return c.Members[i].Key < c.Members[j].Key })
	c.Removed = slices.Clone(r.Removed)
	sort.Strings(c.Removed)
	if !withSig {
		c.Sig = ""
	}
	data, _ := json.Marshal(c)
	return data
}

// Hash is what the next roster names as its predecessor.
func (r *Roster) Hash() string {
	sum := sha256.Sum256(r.canonical(true))
	return hex.EncodeToString(sum[:])
}

// Sign makes id the roster's signer.
func (r *Roster) Sign(id *Identity) {
	r.Signer = EncodeKey(id.Public)
	r.Sig = base64.StdEncoding.EncodeToString(id.Sign(r.canonical(false)))
}

func (r *Roster) verifySig() error {
	pub, err := DecodeKey(r.Signer)
	if err != nil {
		return fmt.Errorf("roster signer: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	if err != nil || !ed25519.Verify(pub, r.canonical(false), sig) {
		return errors.New("the roster's signature does not verify")
	}
	return nil
}

// Valid checks a roster against the one before it: the same swarm, the next
// epoch, naming the previous hash, signed by a key the previous roster lists.
// prev is nil for a genesis roster, which must be signed by its only member.
func (r *Roster) Valid(prev *Roster) error {
	if err := r.verifySig(); err != nil {
		return err
	}
	if prev == nil {
		if r.Epoch != 1 || r.Prev != "" || len(r.Members) != 1 || r.Members[0].Key != r.Signer {
			return errors.New("a first roster must hold only the key that signed it")
		}
		return nil
	}
	switch {
	case r.Swarm != prev.Swarm:
		return errors.New("the roster belongs to another swarm")
	case r.Epoch != prev.Epoch+1:
		return fmt.Errorf("roster epoch %d does not follow %d", r.Epoch, prev.Epoch)
	case r.Prev != prev.Hash():
		return errors.New("the roster does not follow the one before it")
	case !prev.HasKey(r.Signer):
		return errors.New("the roster is signed by a key that was not a member")
	}
	for _, m := range r.Members {
		if slices.Contains(r.Removed, m.Key) {
			return errors.New("the roster lists a removed key as a member")
		}
	}
	return nil
}

// Next is the roster after this one, changed by edit and signed by id.
func (r *Roster) Next(id *Identity, edit func(*Roster)) *Roster {
	n := &Roster{Swarm: r.Swarm, Epoch: r.Epoch + 1, Prev: r.Hash(), Home: r.Home,
		Members: slices.Clone(r.Members), Removed: slices.Clone(r.Removed)}
	edit(n)
	n.Sign(id)
	return n
}

// Add puts a key on the roster.
func (r *Roster) Add(key, role string) {
	if r.HasKey(key) {
		return
	}
	r.Members = append(r.Members, RosterMember{Key: key, Role: role})
}

// Remove takes a key off for good: a removed key can never come back.
func (r *Roster) Remove(key string) {
	r.Members = slices.DeleteFunc(r.Members, func(m RosterMember) bool { return m.Key == key })
	if !slices.Contains(r.Removed, key) {
		r.Removed = append(r.Removed, key)
	}
}

// HasKey reports whether key is a current member.
func (r *Roster) HasKey(key string) bool {
	return r != nil && slices.ContainsFunc(r.Members, func(m RosterMember) bool { return m.Key == key })
}

// Member returns the roster entry for an agent id.
func (r *Roster) Member(id string) (RosterMember, bool) {
	if r == nil {
		return RosterMember{}, false
	}
	for _, m := range r.Members {
		if pub, err := DecodeKey(m.Key); err == nil && IDOf(pub) == id {
			return m, true
		}
	}
	return RosterMember{}, false
}

// Has reports whether an agent id is a current member.
func (r *Roster) Has(id string) bool {
	_, ok := r.Member(id)
	return ok
}

// WasRemoved reports whether an agent id was ever removed.
func (r *Roster) WasRemoved(id string) bool {
	if r == nil {
		return false
	}
	for _, key := range r.Removed {
		if pub, err := DecodeKey(key); err == nil && IDOf(pub) == id {
			return true
		}
	}
	return false
}

// IDs lists the agent ids on the roster.
func (r *Roster) IDs() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, m := range r.Members {
		if pub, err := DecodeKey(m.Key); err == nil {
			out = append(out, IDOf(pub))
		}
	}
	sort.Strings(out)
	return out
}
