// Package session persists conversation history as append-only JSONL files
// with a meta sidecar carrying the summary and logical truncation offset.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cyqlelabs/factor/internal/provider"
)

type meta struct {
	Skip    int    `json:"skip"`
	Summary string `json:"summary,omitempty"`
}

// Store is safe for concurrent use; operations on different sessions do not
// block each other.
type Store struct {
	dir   string
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, locks: map[string]*sync.Mutex{}}, nil
}

func (s *Store) lock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	return l
}

// sanitizeKey turns a session key into a safe filename.
func sanitizeKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if out == "" {
		out = "default"
	}
	return out
}

func (s *Store) historyPath(key string) string {
	return filepath.Join(s.dir, sanitizeKey(key)+".jsonl")
}

func (s *Store) metaPath(key string) string {
	return filepath.Join(s.dir, sanitizeKey(key)+".meta.json")
}

// Append adds one message to the session log.
func (s *Store) Append(key string, msg provider.Message) error {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.historyPath(key), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) readMeta(key string) meta {
	var m meta
	data, err := os.ReadFile(s.metaPath(key))
	if err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func (s *Store) writeMeta(key string, m meta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := s.metaPath(key) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(key))
}

func (s *Store) readAll(key string) ([]provider.Message, error) {
	f, err := os.Open(s.historyPath(key))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []provider.Message
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg provider.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // skip torn/corrupt lines rather than losing the session
		}
		out = append(out, msg)
	}
	return out, scanner.Err()
}

// History returns the live (non-truncated) portion of the session.
func (s *Store) History(key string) ([]provider.Message, error) {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	msgs, err := s.readAll(key)
	if err != nil {
		return nil, err
	}
	m := s.readMeta(key)
	if m.Skip >= len(msgs) {
		return nil, nil
	}
	return msgs[m.Skip:], nil
}

// Summary returns the rolling summary of truncated history.
func (s *Store) Summary(key string) string {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	return s.readMeta(key).Summary
}

// SetSummary records a summary and logically truncates all but the last
// keepLast messages (counted at call time).
func (s *Store) SetSummary(key, summary string, keepLast int) error {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	msgs, err := s.readAll(key)
	if err != nil {
		return err
	}
	skip := len(msgs) - keepLast
	if skip < 0 {
		skip = 0
	}
	return s.writeMeta(key, meta{Skip: skip, Summary: summary})
}

// SetSummaryAt records a summary with an absolute physical skip offset.
// Because the log is append-only, an offset computed from an earlier
// snapshot stays valid even if messages were appended meanwhile — this is
// what compaction must use so a mid-summarize append can never shift the
// cut past the chosen turn-safe boundary.
func (s *Store) SetSummaryAt(key, summary string, skip int) error {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	if skip < 0 {
		skip = 0
	}
	return s.writeMeta(key, meta{Skip: skip, Summary: summary})
}

// Skip returns the current physical truncation offset.
func (s *Store) Skip(key string) int {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	return s.readMeta(key).Skip
}

// SetSkip logically truncates the first skip messages, keeping the summary.
func (s *Store) SetSkip(key string, skip int) error {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	m := s.readMeta(key)
	m.Skip = skip
	return s.writeMeta(key, m)
}

// TotalLen returns the physical message count (including truncated ones).
func (s *Store) TotalLen(key string) (int, error) {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	msgs, err := s.readAll(key)
	return len(msgs), err
}

// Clear removes a session entirely.
func (s *Store) Clear(key string) error {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	for _, p := range []string{s.historyPath(key), s.metaPath(key)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// List returns all session keys (sanitized form).
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".jsonl") {
			out = append(out, strings.TrimSuffix(name, ".jsonl"))
		}
	}
	return out, nil
}

// Compact physically rewrites the log dropping truncated messages.
func (s *Store) Compact(key string) error {
	l := s.lock(key)
	l.Lock()
	defer l.Unlock()
	msgs, err := s.readAll(key)
	if err != nil {
		return err
	}
	m := s.readMeta(key)
	if m.Skip <= 0 {
		return nil
	}
	if m.Skip > len(msgs) {
		m.Skip = len(msgs)
	}
	live := msgs[m.Skip:]
	var buf strings.Builder
	for _, msg := range live {
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	tmp := s.historyPath(key) + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.historyPath(key)); err != nil {
		return err
	}
	m.Skip = 0
	if err := s.writeMeta(key, m); err != nil {
		return fmt.Errorf("compact wrote history but not meta: %w", err)
	}
	return nil
}
