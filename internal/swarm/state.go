package swarm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Grant is what one member's tasks may do on this machine. Each agent keeps
// its own grants: the roster says who belongs, never what they may do.
type Grant struct {
	// Tools limits the member's tasks to these tools. Empty means every tool
	// except the ones no remote turn ever gets — and on a machine where the
	// shell runs as root, that is the whole machine.
	Tools []string `json:"tools,omitempty"`
	// Forward lets the member's tasks pass work on to a third agent.
	Forward bool `json:"forward,omitempty"`
}

// Peer is this agent's own record of another member.
type Peer struct {
	// Name is what this agent calls it. Members tell each other their names
	// over the tunnel; the user can rename one here without telling anyone.
	Name  string `json:"name,omitempty"`
	Grant Grant  `json:"grant"`
	// Addrs are where the member said it can be reached directly on its
	// network, signed by its key.
	Addrs []string `json:"addrs,omitempty"`
}

// State is everything this agent keeps about its swarm.
type State struct {
	Rendezvous string           `json:"rendezvous,omitempty"`
	Roster     *Roster          `json:"roster,omitempty"`
	Peers      map[string]*Peer `json:"peers,omitempty"`
	// Contexts maps an origin session, a member and an audience to the A2A
	// context the conversation with that member runs in.
	Contexts map[string]string `json:"contexts,omitempty"`
	// Refreshed is when the rendezvous last confirmed the roster.
	Refreshed time.Time `json:"refreshed,omitempty"`
	// Frozen is why membership changes are stopped, when two rosters with
	// the same epoch have been seen.
	Frozen string `json:"frozen,omitempty"`
}

// StaleAfter is how old the roster may get before this agent stops taking
// tasks: a removal it has not heard about must not stay unenforced forever.
const StaleAfter = 7 * 24 * time.Hour

// Book keeps State on disk.
type Book struct {
	path string
	mu   sync.Mutex
	st   State
}

const stateFile = "state.json"

// OpenBook reads the state in dir.
func OpenBook(dir string) (*Book, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := &Book{path: filepath.Join(dir, stateFile)}
	data, err := os.ReadFile(b.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &b.st); err != nil {
			return nil, fmt.Errorf("%s: %w", b.path, err)
		}
	}
	return b, nil
}

// Read returns a copy of the state.
func (b *Book) Read() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st.clone()
}

// Update changes the state and saves it, or leaves both alone on an error.
func (b *Book) Update(edit func(*State) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	next := b.st.clone()
	if err := edit(&next); err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return err
	}
	b.st = next
	return nil
}

func (s State) clone() State {
	c := s
	if s.Roster != nil {
		r := *s.Roster
		r.Members = slices.Clone(s.Roster.Members)
		r.Removed = slices.Clone(s.Roster.Removed)
		c.Roster = &r
	}
	c.Peers = make(map[string]*Peer, len(s.Peers))
	for k, v := range s.Peers {
		p := *v
		p.Grant.Tools = slices.Clone(v.Grant.Tools)
		p.Addrs = slices.Clone(v.Addrs)
		c.Peers[k] = &p
	}
	c.Contexts = make(map[string]string, len(s.Contexts))
	for k, v := range s.Contexts {
		c.Contexts[k] = v
	}
	return c
}

// Joined reports whether this agent belongs to a swarm.
func (s State) Joined() bool { return s.Roster != nil }

// Peer returns this agent's record of a member, or a blank one.
func (s State) Peer(id string) Peer {
	if p, ok := s.Peers[id]; ok && p != nil {
		return *p
	}
	return Peer{}
}

// Label is what to call a member: its name, or the start of its id.
func (s State) Label(id string) string {
	if name := s.Peer(id).Name; name != "" {
		return name
	}
	return id[:min(len(id), 8)]
}

// Member is one other agent, resolved for display and dispatch.
type Member struct {
	ID    string
	Name  string
	Role  string
	Grant Grant
}

// Members lists the other agents on the roster, sorted by name.
func (s State) Members(self string) []Member {
	var out []Member
	for _, id := range s.Roster.IDs() {
		if id == self {
			continue
		}
		rm, _ := s.Roster.Member(id)
		out = append(out, Member{ID: id, Name: s.Label(id), Role: rm.Role, Grant: s.Peer(id).Grant})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve finds a member by name, or by the start of its id.
func (s State) Resolve(self, who string) (Member, error) {
	who = strings.TrimSpace(who)
	if who == "" {
		return Member{}, errors.New("say which agent")
	}
	members := s.Members(self)
	for _, m := range members {
		if strings.EqualFold(m.Name, who) || m.ID == strings.ToLower(who) {
			return m, nil
		}
	}
	var hits []Member
	for _, m := range members {
		if strings.Contains(strings.ToLower(m.Name), strings.ToLower(who)) ||
			strings.HasPrefix(m.ID, strings.ToLower(who)) {
			hits = append(hits, m)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	var names []string
	for _, m := range members {
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return Member{}, errors.New("this swarm has no other members yet")
	}
	return Member{}, fmt.Errorf("no single member matches %q; the members are: %s", who, strings.Join(names, ", "))
}
