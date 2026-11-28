package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
)

// A task handed to another machine reaches that machine's user through two
// of its tools — voice_say in its voice, message_user in a written chat —
// and the agent handing it over sees neither. Asked to have a machine with no
// voice say hello, the receiving end of the first live test spent twenty-five
// tool calls looking for a way to, and the follow-up that named voice_say had
// it rewire its media player. So the members tell each other, over the
// tunnel, what those two tools would accept right now, and swarm_send lists
// it for the agent deciding what to ask for.
const reachPath = "swarm/v1/reach"

// sayToolName is the voice channel's tool, named here rather than imported:
// the swarm does not depend on a channel.
const sayToolName = "voice_say"

// Reach is what a member's tasks can reach of its own user.
type Reach struct {
	// Voice says voice_say would speak: the voice channel is running. A
	// machine can have speakers and no voice; only the voice says things.
	Voice bool `json:"voice,omitempty"`
	// Chats are the written channels message_user would write to: running,
	// and holding a chat the user has written in.
	Chats []string `json:"chats,omitempty"`
}

// reachMessage is both halves of the exchange. Reach is a pointer so that an
// answer without one — an older Factor, whose JSON-RPC handler answers any
// path it does not know — is not read as a member that reaches nothing.
type reachMessage struct {
	Reach *Reach `json:"reach"`
}

// Every chat name lands in a tool description another agent's model reads,
// so a member may only name channels the way Factor names them.
var chatName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

const maxChats = 8

func (r Reach) clean() Reach {
	var chats []string
	for _, c := range r.Chats {
		if chatName.MatchString(c) && !slices.Contains(chats, c) && len(chats) < maxChats {
			chats = append(chats, c)
		}
	}
	slices.Sort(chats)
	return Reach{Voice: r.Voice, Chats: chats}
}

func (r Reach) equal(o Reach) bool {
	return r.Voice == o.Voice && slices.Equal(r.Chats, o.Chats)
}

// SetReach is how this agent reads what its tasks can reach now. The gateway
// sets it before Connect.
func (s *Service) SetReach(read func() Reach) { s.reach = read }

// reachFor is what this agent tells one member: only what that member's
// tasks are allowed to use here, so a guest is told nothing it cannot have.
func (s *Service) reachFor(st State, peer string) Reach {
	if s.reach == nil {
		return Reach{}
	}
	here := s.reach().clean()
	rm, _ := st.Roster.Member(peer)
	may := allowed(st.Peer(peer).Grant, rm.Role, false)
	out := Reach{Voice: here.Voice && may(sayToolName)}
	if may(agent.MessageToolName) {
		out.Chats = here.Chats
	}
	return out
}

// errSameReach leaves the state file alone when a member said what it said
// last time: the list sits in a tool description, and a rewrite that changed
// nothing must not be the reason a cached prefix stops matching.
var errSameReach = errors.New("reach unchanged")

// heard records what a member said its tasks can reach.
func (s *Service) heard(peer string, r Reach) {
	r = r.clean()
	_ = s.book.Update(func(st *State) error {
		if !st.Roster.Has(peer) {
			return errors.New("not a member")
		}
		p := st.peer(peer)
		if p.Reach != nil && p.Reach.equal(r) {
			return errSameReach
		}
		p.Reach = &r
		return nil
	})
}

// exchangeReach tells every member what its tasks can reach here and records
// what each says back. Both ends run it whenever a roster is adopted — on
// every link-up and on every hourly confirmation — so a member that restarts
// with other channels is heard at once by the members it greets.
func (s *Service) exchangeReach() {
	transport, _, _ := s.net()
	if transport == nil {
		return
	}
	st := s.book.Read()
	for _, m := range st.Members(s.id.ID) {
		s.wg.Add(1)
		go func(peer string, mine Reach) {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(s.bg, 15*time.Second)
			defer cancel()
			theirs, err := askReach(ctx, transport, peer, mine)
			if err != nil {
				slog.Debug("swarm: no reach from a member", "peer", peer, "error", err)
				return
			}
			s.heard(peer, theirs)
		}(m.ID, s.reachFor(st, m.ID))
	}
}

func askReach(ctx context.Context, transport Transport, peer string, mine Reach) (Reach, error) {
	hc, base, err := transport.HTTPClient(peer)
	if err != nil {
		return Reach{}, err
	}
	body, err := json.Marshal(reachMessage{Reach: &mine})
	if err != nil {
		return Reach{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, join(base, reachPath), bytes.NewReader(body))
	if err != nil {
		return Reach{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Reach{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Reach{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var in reachMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&in); err != nil {
		return Reach{}, err
	}
	if in.Reach == nil {
		return Reach{}, errors.New("the answer says nothing about reach")
	}
	return *in.Reach, nil
}

// serveReach records what a member says it reaches and answers with what it
// can reach here.
func (s *Service) serveReach(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.peerOf(r)
	if !ok {
		http.Error(w, "unknown agent", http.StatusUnauthorized)
		return
	}
	var in reachMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || in.Reach == nil {
		http.Error(w, "not a reach announcement", http.StatusBadRequest)
		return
	}
	s.heard(peer, *in.Reach)
	mine := s.reachFor(s.book.Read(), peer)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reachMessage{Reach: &mine})
}

// reachText lists what each member last said its tasks can reach, for
// swarm_send's description. That description is part of every request's
// cached prefix, so the text holds nothing that moves on its own: members in
// name order, and nothing but what they said.
func (s *Service) reachText() string {
	st := s.book.Read()
	members := st.Members(s.id.ID)
	if len(members) == 0 {
		return ""
	}
	parts := make([]string, 0, len(members))
	for _, m := range members {
		parts = append(parts, m.Name+": "+reachPhrase(st.Peer(m.ID).Reach))
	}
	return " What each agent here can reach of its own user, as it last said: " + strings.Join(parts, "; ") +
		". Ask an agent only for what it can reach: one without a voice cannot say anything aloud, and naming " +
		"a tool it lacks does not give it one, so tell the user so, or have it write the message in its chat " +
		"instead."
}

func reachPhrase(r *Reach) string {
	switch {
	case r == nil:
		return "has not said yet"
	case r.Voice && len(r.Chats) > 0:
		return "its voice and a written chat on " + strings.Join(r.Chats, ", ")
	case r.Voice:
		return "its voice but no written chat"
	case len(r.Chats) > 0:
		return "a written chat on " + strings.Join(r.Chats, ", ") + " but no voice"
	}
	return "neither a voice nor a written chat"
}
