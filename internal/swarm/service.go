package swarm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	fa2a "github.com/cyqlelabs/factor/internal/a2a"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/decision/local"
	"github.com/cyqlelabs/factor/internal/tools"
)

// minFreeMB is the memory a machine must have free to take a task: under it
// a turn would push the box into swap, and the owner's own work with it.
const minFreeMB = 300

// Service is this agent's membership in a swarm: the server other members
// reach, the tasks it hands out, and the tools the model drives both with.
type Service struct {
	cfg     config.SwarmConfig
	dir     string
	id      *Identity
	book    *Book
	turns   Turns
	bus     *bus.MessageBus
	store   *fa2a.Store
	server  *fa2a.Server
	deleg   *ledger
	version string
	now     func() time.Time

	transport Transport
	// peerOf names the agent behind an inbound request. The tunnel reads it
	// off the pinned certificate; a test can stand in for it.
	peerOf func(*http.Request) (string, bool)

	bg     context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Dir is where the swarm keeps its key, its state and its tasks.
func Dir() string { return filepath.Join(config.Home(), "swarm") }

// New opens this agent's swarm state in dir, making its key on first use.
func New(cfg config.SwarmConfig, dir, version string, turns Turns, b *bus.MessageBus) (*Service, error) {
	id, err := LoadIdentity(dir)
	if err != nil {
		return nil, fmt.Errorf("swarm identity: %w", err)
	}
	book, err := OpenBook(dir)
	if err != nil {
		return nil, fmt.Errorf("swarm state: %w", err)
	}
	store, err := fa2a.OpenStore(filepath.Join(dir, "tasks"), fa2a.KeepTasks)
	if err != nil {
		return nil, fmt.Errorf("swarm tasks: %w", err)
	}
	delegDir := filepath.Join(dir, "delegations")
	if err := os.MkdirAll(delegDir, 0o700); err != nil {
		return nil, err
	}
	bg, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, dir: dir, id: id, book: book, turns: turns, bus: b, store: store,
		deleg: &ledger{dir: delegDir}, version: version, now: time.Now, bg: bg, cancel: cancel}
	s.peerOf = s.tlsPeer
	s.server = fa2a.NewServer(s, store, fa2a.Options{
		Self:       id.ID,
		Concurrent: cfg.RemoteTasks,
		Queue:      cfg.RemoteQueue,
		MinFreeMB:  minFreeMB,
		FreeMB: func() int {
			if mb, ok := local.AvailableMB(); ok {
				return mb
			}
			return -1
		},
		Card: fa2a.Card(s.Name(), version, "https://"+id.ID+HostSuffix+"/"),
	})
	return s, nil
}

// ID is this agent's id.
func (s *Service) ID() string { return s.id.ID }

// Identity is this agent's key.
func (s *Service) Identity() *Identity { return s.id }

// Book is this agent's swarm state.
func (s *Service) Book() *Book { return s.book }

// Name is what this agent calls itself to the others.
func (s *Service) Name() string {
	if s.cfg.Name != "" {
		return s.cfg.Name
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return s.id.ID[:8]
}

// SetTransport is how this agent reaches the others.
func (s *Service) SetTransport(t Transport) { s.transport = t }

// Handler serves A2A to the members. It is mounted on the tunnel, never on a
// listening port: only a connection whose certificate the roster lists gets
// this far.
func (s *Service) Handler() http.Handler { return s.server.Handler(s.peerOf) }

func (s *Service) tlsPeer(r *http.Request) (string, bool) {
	if r.TLS == nil {
		return "", false
	}
	peer, ok := PeerID(*r.TLS)
	if !ok || !s.book.Read().Roster.Has(peer) {
		return "", false
	}
	return peer, true
}

// IsMember reports whether an agent id is on the roster, which is the whole
// test the tunnel applies to a connection.
func (s *Service) IsMember(peer string) bool {
	return peer != s.id.ID && s.book.Read().Roster.Has(peer)
}

// MemberOf names the member a session works for: the agent behind a remote
// task's session, or "" for the user's own. It is how a member's daily cap
// finds the spend it limits.
func MemberOf(sessionKey string) string {
	rest, ok := strings.CutPrefix(sessionKey, "a2a:")
	if !ok {
		return ""
	}
	peer, _, _ := strings.Cut(rest, ":")
	return peer
}

// Start reconciles what the last process left unfinished and keeps an eye on
// results held back for company.
func (s *Service) Start() {
	s.reconcile()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(pollEvery)
		defer ticker.Stop()
		for {
			select {
			case <-s.bg.Done():
				return
			case <-ticker.C:
				s.redeliver()
			}
		}
	}()
}

// Close ends the tasks this agent is running for others and stops following
// the ones it sent.
func (s *Service) Close() {
	s.cancel()
	s.server.Close()
	s.wg.Wait()
}

// Tools are the two the model drives the swarm with.
func (s *Service) Tools() []tools.Tool { return []tools.Tool{&sendTool{s: s}, &adminTool{s: s}} }

func (s *Service) listText() string {
	st := s.book.Read()
	if !st.Joined() {
		return "This agent is not in a swarm. The user can start one by asking to add another machine."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This agent is %s (id %s), in a swarm of %d at roster epoch %d.\n",
		s.Name(), s.id.ID, len(st.Roster.Members), st.Roster.Epoch)
	if st.Frozen != "" {
		fmt.Fprintf(&b, "Membership changes are frozen: %s\n", st.Frozen)
	}
	members := st.Members(s.id.ID)
	if len(members) == 0 {
		b.WriteString("It has no other members yet.")
		return b.String()
	}
	for _, m := range members {
		fmt.Fprintf(&b, "- %s (id %s, %s): %s\n", m.Name, m.ID, m.Role, grantText("its", m.Grant))
	}
	return strings.TrimSpace(b.String())
}

// statusText is the latest word on everything handed out in the last day:
// the last status each member reported, which the stream or the poll
// following each task keeps current.
func (s *Service) statusText() string {
	st := s.book.Read()
	var lines []string
	for _, d := range s.deleg.all() {
		if d.Delivered && s.now().Sub(d.Updated) > 24*time.Hour {
			continue
		}
		line := fmt.Sprintf("- %s, %q: %s", st.Label(d.Peer), d.Request, d.State)
		if d.Said != "" {
			line += " — " + d.Said
		}
		if d.State == stQuestion {
			line += fmt.Sprintf(" (task %s)", d.Task)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "Nothing has been handed to another agent in the last day."
	}
	return strings.Join(lines, "\n")
}
