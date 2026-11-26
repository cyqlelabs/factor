package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// errGuest is what a guest gets for trying to change the swarm.
var errGuest = errors.New("a guest of the swarm cannot change who belongs to it")

// guest reports whether this agent holds only a guest's place.
func (s *Service) guest() bool {
	m, ok := s.book.Read().Roster.Member(s.id.ID)
	return ok && m.Role == RoleGuest
}

// Found starts a new swarm with this agent as its only member, at the
// rendezvous it is connected to. Adding the first other machine calls it.
func (s *Service) Found(ctx context.Context) error {
	if s.book.Read().Joined() {
		return nil
	}
	_, link, _ := s.net()
	if link == nil {
		return errors.New(s.offlineText())
	}
	if err := link.WaitOnline(ctx); err != nil {
		return fmt.Errorf("the rendezvous is not reachable: %w", err)
	}
	id := NewSwarmID()
	if err := s.book.Update(func(st *State) error {
		st.Joining = id
		return nil
	}); err != nil {
		return err
	}
	return s.commit(ctx, Genesis(s.id, id, link.url))
}

// Admit puts a new member on the roster under the name this agent will call
// it, with the default grant for its role: a member of the swarm, or a guest
// — somebody else's agent, held to a few tools and a memory space of its own.
func (s *Service) Admit(ctx context.Context, key, name, role string) error {
	if role == "" {
		role = RoleMember
	}
	if role != RoleMember && role != RoleGuest {
		return fmt.Errorf("unknown role %q", role)
	}
	st := s.book.Read()
	if !st.Joined() {
		return errors.New("this agent is not in a swarm")
	}
	if s.guest() {
		return errGuest
	}
	pub, err := DecodeKey(key)
	if err != nil {
		return err
	}
	peer := IDOf(pub)
	if st.Roster.WasRemoved(peer) {
		return errors.New("that agent was removed from the swarm and cannot come back")
	}
	if name != "" {
		if err := s.book.Update(func(st *State) error {
			st.peer(peer).Name = name
			return nil
		}); err != nil {
			return err
		}
	}
	if st.Roster.Has(peer) {
		return nil
	}
	return s.commit(ctx, st.Roster.Next(s.id, func(r *Roster) { r.Add(key, role) }))
}

// Remove takes a member off the roster for good. Every member drops it, the
// rendezvous refuses it from then on, and its tasks here are cancelled.
func (s *Service) Remove(ctx context.Context, peer string) error {
	if s.guest() {
		return errGuest
	}
	st := s.book.Read()
	m, ok := st.Roster.Member(peer)
	if !ok {
		return fmt.Errorf("%s is not a member", peer)
	}
	if peer == s.id.ID {
		return errors.New("an agent cannot remove itself; remove it from another member")
	}
	return s.commit(ctx, st.Roster.Next(s.id, func(r *Roster) { r.Remove(m.Key) }))
}

func (s *Service) removeMember(ctx context.Context, who string) *tools.Result {
	st := s.book.Read()
	m, err := st.Resolve(s.id.ID, who)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.Remove(ctx, m.ID); err != nil {
		return tools.Errorf("could not remove %s: %v", m.Name, err)
	}
	return tools.Textf("%s is out of the swarm. Every member drops it and refuses it from now on.", m.Name)
}
