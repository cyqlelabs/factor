package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/cyqlelabs/factor/internal/bus"
)

// Connect brings this agent online in its swarm: one outbound link to the
// rendezvous and the tunnel over it. It returns at once; the link reconnects
// on its own for as long as the service runs.
func (s *Service) Connect() {
	url := s.rendezvousURL()
	if url == "" {
		s.offline = "no rendezvous is configured (swarm.rendezvous)"
		return
	}
	link := NewLink(url, s.id, s.swarmID)
	link.OnRoster(func(r *Roster) { s.adopt(r, true) })
	link.rostered = func() bool { return s.book.Read().Joined() }
	tunnel := NewTunnel(s.id, link, s.IsMember, s.Handler())
	ctx, cancel := context.WithCancel(s.bg)
	s.netMu.Lock()
	s.link, s.tunnel, s.transport, s.linkCancel = link, tunnel, tunnel, cancel
	s.netMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		link.Run(ctx)
	}()
	if s.cfg.LAN {
		s.startLAN(ctx, tunnel)
	}
}

// disconnect ends the link and the tunnel over it.
func (s *Service) disconnect() {
	s.netMu.Lock()
	cancel, tunnel := s.linkCancel, s.tunnel
	s.link, s.tunnel, s.linkCancel, s.lan = nil, nil, nil, nil
	if tunnel != nil && s.transport == Transport(tunnel) {
		s.transport = nil
	}
	s.netMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if tunnel != nil {
		tunnel.Close()
	}
}

// rendezvousURL is where this swarm meets: the roster's home once joined,
// the configured one before.
func (s *Service) rendezvousURL() string {
	st := s.book.Read()
	switch {
	case st.Roster != nil && st.Roster.Home != "":
		return st.Roster.Home
	case st.Rendezvous != "":
		return st.Rendezvous
	}
	return s.cfg.Rendezvous
}

func (s *Service) swarmID() string {
	st := s.book.Read()
	if st.Roster != nil {
		return st.Roster.Swarm
	}
	return st.Joining
}

// verdict is what a roster from the rendezvous means against the one held.
type verdict int

const (
	takeIt  verdict = iota // a newer roster that checks out
	confirm                // the roster already held
	fork                   // another roster at the same epoch
)

// judge decides what to do with r, given the roster held (nil while
// joining).
func (s *Service) judge(cur *Roster, joining string, r *Roster) (verdict, error) {
	switch {
	case cur == nil:
		if r.Swarm != joining || !r.Has(s.id.ID) {
			return 0, errors.New("a roster for a swarm this agent is not joining")
		}
		return takeIt, r.VerifySig()
	case r.Swarm != cur.Swarm:
		return 0, errors.New("a roster for another swarm")
	case r.Epoch < cur.Epoch:
		return 0, fmt.Errorf("a rollback to epoch %d from %d", r.Epoch, cur.Epoch)
	case r.Epoch == cur.Epoch && r.Hash() == cur.Hash():
		return confirm, nil
	case r.Epoch == cur.Epoch:
		return fork, nil
	case r.Epoch == cur.Epoch+1:
		return takeIt, r.Valid(cur)
	}
	// Changes made while this agent was away. The rendezvous keeps only the
	// head, so the steps between cannot be checked one by one; the signature
	// and the signer's membership can.
	if err := r.VerifySig(); err != nil {
		return 0, err
	}
	if !cur.HasKey(r.Signer) && !r.HasKey(r.Signer) {
		return 0, errors.New("signed by a key neither roster lists")
	}
	return takeIt, nil
}

// adopt takes a roster the rendezvous sent. It keeps the highest committed
// epoch and refuses a rollback; two different rosters at one epoch freeze
// membership changes and tell the user, and the last trusted roster stays in
// force meanwhile.
func (s *Service) adopt(r *Roster, fromRendezvous bool) {
	if r == nil {
		return
	}
	var removed []string
	var alert string
	err := s.book.Update(func(st *State) error {
		v, err := s.judge(st.Roster, st.Joining, r)
		if err != nil {
			return err
		}
		if v == fork {
			// A fork is no confirmation of anything: it neither refreshes
			// the roster nor replaces it.
			if st.Frozen == "" {
				st.Frozen = fmt.Sprintf("two different rosters claim epoch %d", r.Epoch)
				alert = st.Frozen
			}
			return nil
		}
		if fromRendezvous {
			st.Refreshed = s.now()
		}
		if v == confirm {
			return nil
		}
		for _, id := range st.Roster.IDs() {
			if !r.Has(id) {
				removed = append(removed, id)
			}
		}
		st.Roster, st.Rendezvous, st.Joining = r, r.Home, ""
		return nil
	})
	if err != nil {
		slog.Warn("swarm: refused a roster", "epoch", r.Epoch, "error", err)
		return
	}
	s.cutOff(removed)
	if alert != "" {
		s.tell("Membership changes in the swarm are frozen: " + alert + ". The rendezvous may be lying or a removed " +
			"member may be trying to fork the swarm. The last roster this agent trusted stays in force.")
	}
	if slices.Contains(removed, s.id.ID) {
		s.tell("This agent has been removed from the swarm.")
	}
	s.learnNames()
}

// cutOff ends what removed members had running here and open to here.
func (s *Service) cutOff(removed []string) {
	_, _, tunnel := s.net()
	for _, id := range removed {
		s.server.CancelPeer(id)
		if tunnel != nil {
			tunnel.Drop(id)
		}
	}
}

// tell reaches the user in the last chat they used.
func (s *Service) tell(text string) {
	if s.notify != nil {
		s.notify(text)
	}
}

// learnNames asks each member it has no name for what it calls itself. The
// name travels over the tunnel, never through the rendezvous.
func (s *Service) learnNames() {
	if _, _, tunnel := s.net(); tunnel == nil {
		return
	}
	s.discoverAll()
	st := s.book.Read()
	for _, m := range st.Members(s.id.ID) {
		if st.Peer(m.ID).Name != "" {
			continue
		}
		s.wg.Add(1)
		go func(peer string) {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(s.bg, 10*time.Second)
			defer cancel()
			name, err := s.askName(ctx, peer)
			if err != nil || name == "" {
				return
			}
			_ = s.book.Update(func(st *State) error {
				if p := st.peer(peer); p.Name == "" {
					p.Name = name
				}
				return nil
			})
		}(m.ID)
	}
}

func (s *Service) askName(ctx context.Context, peer string) (string, error) {
	_, _, tunnel := s.net()
	if tunnel == nil {
		return "", errOffline
	}
	hc, base, err := tunnel.HTTPClient(peer)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, join(base, a2asrv.WellKnownAgentCardPath), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return "", err
	}
	return card.Name, nil
}

// commit makes next the swarm's roster, through the rendezvous, which takes
// it only on top of the head it holds.
func (s *Service) commit(ctx context.Context, next *Roster) error {
	if st := s.book.Read(); st.Frozen != "" {
		return fmt.Errorf("membership changes are frozen: %s", st.Frozen)
	}
	_, link, _ := s.net()
	if link == nil {
		return errors.New(s.offlineText())
	}
	if err := link.Commit(ctx, next); err != nil {
		return err
	}
	s.adopt(next, true)
	return nil
}

// Status is one line on where the swarm stands, for /health and the tray.
func (s *Service) Status() string {
	st := s.book.Read()
	if !st.Joined() && st.Joining == "" {
		return "not in a swarm"
	}
	members := 0
	if st.Roster != nil {
		members = len(st.Roster.Members)
	}
	_, link, _ := s.net()
	if link == nil {
		return s.offlineText()
	}
	state, _ := link.Status()
	if st.Frozen != "" {
		state += ", membership frozen"
	}
	line := fmt.Sprintf("%s, %d members", state, members)
	if direct := s.directCount(); direct > 0 {
		line += fmt.Sprintf(", %d direct", direct)
	}
	return line
}

func (s *Service) offlineText() string {
	if s.offline != "" {
		return "offline: " + s.offline
	}
	return "offline"
}

// OnNotice sets how the service reaches the user unprompted.
func (s *Service) OnNotice(fn func(string)) { s.notify = fn }

// NoticeTo sends notices to the last chat the user used.
func NoticeTo(b *bus.MessageBus, last func() (string, string, bool)) func(string) {
	return func(text string) {
		if ch, chat, ok := last(); ok {
			b.PublishOutbound(bus.OutboundMessage{Channel: ch, ChatID: chat, Content: text})
		}
	}
}
