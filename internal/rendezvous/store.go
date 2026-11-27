// Package rendezvous is `factor rendezvous`: the meeting point of a wire. It
// knows which agent keys are online, keeps each swarm's roster head, holds
// pairing mailboxes, and relays bytes between two keys on the same roster —
// bytes the agents encrypt end to end, so it forwards what it cannot read.
package rendezvous

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cyqlelabs/factor/internal/swarm/wire"
)

// marker is the file that says a directory is a rendezvous store. A store
// without it was lost or never made, and a rendezvous that started over
// would take the word of whichever member reconnected first — including one
// removed a minute earlier, who could sign a roster of its own.
const marker = "FACTOR-RENDEZVOUS"

// Store keeps each swarm's roster head on disk. It is the one thing the
// rendezvous stores durably, and it is written before any change is
// acknowledged or pushed.
type Store struct {
	dir string
	// refusing says why roster changes are refused, when they are.
	refusing string

	mu     sync.Mutex
	heads  map[string]*wire.Roster
	broken map[string]string
}

// OpenStore opens the store in dir. create makes a new one where none
// exists; without it a missing store refuses changes rather than starting
// over.
func OpenStore(dir string, create bool) (*Store, error) {
	s := &Store{dir: dir, heads: map[string]*wire.Roster{}, broken: map[string]string{}}
	_, err := os.Stat(filepath.Join(dir, marker))
	switch {
	case errors.Is(err, os.ErrNotExist) && create:
		if err := os.MkdirAll(filepath.Join(dir, "rosters"), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, marker), []byte("a factor rendezvous store\n"), 0o600); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		s.refusing = fmt.Sprintf("there is no rendezvous store at %s; it was lost or never made", dir)
		return s, nil
	case err != nil:
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "rosters"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		r, err := readRoster(filepath.Join(dir, "rosters", e.Name()), id)
		if err != nil {
			s.broken[id] = err.Error()
			continue
		}
		s.heads[id] = r
	}
	return s, nil
}

// readRoster reads one stored head, which must be the roster of swarm id.
func readRoster(path, id string) (*wire.Roster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r wire.Roster
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r.Swarm != id {
		return nil, errors.New("the file holds another swarm's roster")
	}
	return &r, nil
}

// Refusing says why the store takes no changes, or "".
func (s *Store) Refusing() string { return s.refusing }

// Head is a swarm's current roster, or nil.
func (s *Store) Head(id string) *wire.Roster {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heads[id]
}

// Swarms counts the swarms the store holds.
func (s *Store) Swarms() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.heads)
}

// Commit makes r the head of its swarm if it follows the current head — or,
// for a swarm the store has never seen, if it is a valid first roster. The
// compare and the write happen under one lock, so two members committing at
// once cannot both win.
func (s *Store) Commit(r *wire.Roster) error {
	if s.refusing != "" {
		return errors.New(s.refusing)
	}
	if !validID(r.Swarm) {
		return errors.New("the swarm id is not one this rendezvous can store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if why, ok := s.broken[r.Swarm]; ok {
		return fmt.Errorf("this swarm's stored roster is damaged (%s); its membership cannot change", why)
	}
	head := s.heads[r.Swarm]
	if err := r.Valid(head); err != nil {
		if head != nil {
			return fmt.Errorf("%w (the current roster is epoch %d)", err, head.Epoch)
		}
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := s.keepRemovals(head, r, data); err != nil {
		return fmt.Errorf("the roster could not be stored: %w", err)
	}
	if err := writeDurable(filepath.Join(s.dir, "rosters", r.Swarm+".json"), data); err != nil {
		return fmt.Errorf("the roster could not be stored: %w", err)
	}
	s.heads[r.Swarm] = r
	return nil
}

// keepRemovals stores r beside each agent it removes. Shown that roster, a
// removed agent can see it is out; shown the head, it would also see every
// change made since, and who joined after it.
func (s *Store) keepRemovals(head, r *wire.Roster, data []byte) error {
	for _, key := range r.Removed {
		pub, err := wire.DecodeKey(key)
		if err != nil {
			continue
		}
		id := wire.IDOf(pub)
		if head.WasRemoved(id) {
			continue
		}
		if err := os.MkdirAll(filepath.Join(s.dir, "removals"), 0o700); err != nil {
			return err
		}
		if err := writeDurable(s.removalPath(r.Swarm, id), data); err != nil {
			return err
		}
	}
	return nil
}

// RemovedBy is the roster that removed an agent from a swarm, or nil.
func (s *Store) RemovedBy(swarm, id string) *wire.Roster {
	if !validID(swarm) || !validID(id) {
		return nil
	}
	r, err := readRoster(s.removalPath(swarm, id), swarm)
	if err != nil {
		return nil
	}
	return r
}

func (s *Store) removalPath(swarm, id string) string {
	return filepath.Join(s.dir, "removals", swarm+"."+id+".json")
}

func validID(id string) bool {
	return id != "" && len(id) <= 64 && !strings.ContainsFunc(id, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < '2' || c > '7')
	})
}

// writeDurable replaces path with data and makes sure the bytes, and the
// rename, are on disk before it returns.
func writeDurable(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
