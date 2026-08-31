package cost

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// Totals is one bucket of spend: what went in, what came out, and what it
// cost. USD stays at zero for tokens nothing priced, so a report can say
// "unpriced" instead of implying "free".
type Totals struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
	// Cached is the part of Input a prompt cache served rather than
	// reprocessed — a subset of Input, never an addition to it. It is
	// recorded because the ratio is the only alarm Factor has for a prefix
	// that stopped being byte-stable: everything upstream of it fails
	// silently and only this number moves.
	Cached int     `json:"cached_input_tokens,omitempty"`
	USD    float64 `json:"usd"`
	Calls  int     `json:"calls"`
}

// Tokens is the whole traffic of a bucket, which is what a status line has
// room for. Cached is left out because it is already inside Input.
func (t Totals) Tokens() int { return t.Input + t.Output }

// CacheHitRate is the share of input served from cache, 0 when nothing was
// read at all.
func (t Totals) CacheHitRate() float64 {
	if t.Input <= 0 {
		return 0
	}
	return float64(t.Cached) / float64(t.Input)
}

func (t *Totals) add(o Totals) {
	t.Input += o.Input
	t.Output += o.Output
	t.Cached += o.Cached
	t.USD += o.USD
	t.Calls += o.Calls
}

// stamped is a session bucket with the time it was last billed, so the
// oldest conversations are the ones dropped when the file is pruned.
type stamped struct {
	Totals
	Last time.Time `json:"last"`
}

// book is the whole ledger as it lives on disk.
type book struct {
	Total    Totals             `json:"total"`
	Days     map[string]Totals  `json:"days"`
	Months   map[string]Totals  `json:"months"`
	Models   map[string]Totals  `json:"models"`
	Sessions map[string]stamped `json:"sessions"`
}

func (b *book) init() {
	if b.Days == nil {
		b.Days = map[string]Totals{}
	}
	if b.Months == nil {
		b.Months = map[string]Totals{}
	}
	if b.Models == nil {
		b.Models = map[string]Totals{}
	}
	if b.Sessions == nil {
		b.Sessions = map[string]stamped{}
	}
}

// How much history the file keeps. Old buckets answer nothing anyone asks,
// and an unbounded map of chat ids is a file that only ever grows.
const (
	keepDays     = 90
	keepMonths   = 24
	keepSessions = 200
)

func dayKey(t time.Time) string   { return t.Format("2006-01-02") }
func monthKey(t time.Time) string { return t.Format("2006-01") }

// entry is one billed call, held until it has been merged into the file.
type entry struct {
	at      time.Time
	session string
	model   string
	totals  Totals
}

// Ledger accumulates spend and keeps it on disk, so "what has this cost me"
// survives a restart. Each call is merged into the file rather than
// overwriting it: the gateway and a terminal session can both be spending at
// once, and neither should erase the other's total.
type Ledger struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	book    book
	pending []entry
	warned  bool
}

// NewLedger opens the ledger at path, reading whatever is already there. A
// file that cannot be read starts empty: losing a total is not worth failing
// startup over.
func NewLedger(path string) *Ledger {
	l := &Ledger{path: path, now: time.Now}
	l.book = l.read()
	return l
}

func (l *Ledger) read() book {
	var b book
	if l.path != "" {
		if data, err := os.ReadFile(l.path); err == nil {
			_ = json.Unmarshal(data, &b)
		}
	}
	b.init()
	return b
}

// Record bills one provider call to a session and flushes it to disk. A
// write that fails keeps the call pending, so the next one carries both —
// worth saying once, because the totals are then only as durable as the
// process, but not worth saying on every call while the disk stays broken.
func (l *Ledger) Record(sessionKey, model string, t Totals) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := entry{at: l.now(), session: sessionKey, model: model, totals: t}
	l.pending = append(l.pending, e)
	apply(&l.book, e)
	if err := l.flushLocked(); err != nil && !l.warned {
		l.warned = true
		slog.Warn("usage totals are not reaching disk", "path", l.path, "error", err)
	}
}

// Flush merges anything still pending into the file.
func (l *Ledger) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushLocked()
}

func (l *Ledger) flushLocked() error {
	if l.path == "" || len(l.pending) == 0 {
		return nil
	}
	merged := l.read()
	for _, e := range l.pending {
		apply(&merged, e)
	}
	prune(&merged)
	if err := l.write(merged); err != nil {
		return err
	}
	l.book, l.pending = merged, nil
	return nil
}

func (l *Ledger) write(b book) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

func apply(b *book, e entry) {
	b.init()
	b.Total.add(e.totals)
	addTo(b.Days, dayKey(e.at), e.totals)
	addTo(b.Months, monthKey(e.at), e.totals)
	if e.model != "" {
		addTo(b.Models, e.model, e.totals)
	}
	if e.session != "" {
		s := b.Sessions[e.session]
		s.add(e.totals)
		s.Last = e.at
		b.Sessions[e.session] = s
	}
}

func addTo(m map[string]Totals, key string, t Totals) {
	cur := m[key]
	cur.add(t)
	m[key] = cur
}

func prune(b *book) {
	trimOldest(b.Days, keepDays)
	trimOldest(b.Months, keepMonths)
	if len(b.Sessions) <= keepSessions {
		return
	}
	keys := make([]string, 0, len(b.Sessions))
	for k := range b.Sessions {
		keys = append(keys, k)
	}
	// Newest first, and the key decides a tie. Sessions billed in the same
	// instant are common — a burst of turns, or a fake clock in a test — and
	// without the tie-break which of them survives comes down to map
	// iteration order, so two processes merging the same ledger would prune
	// it differently.
	sort.Slice(keys, func(i, j int) bool {
		a, z := b.Sessions[keys[i]].Last, b.Sessions[keys[j]].Last
		if a.Equal(z) {
			return keys[i] < keys[j]
		}
		return a.After(z)
	})
	for _, k := range keys[keepSessions:] {
		delete(b.Sessions, k)
	}
}

// trimOldest keeps the newest n buckets of a date-keyed map, which sorts
// lexically because the keys are ISO dates.
func trimOldest(m map[string]Totals, n int) {
	if len(m) <= n {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, k := range keys[n:] {
		delete(m, k)
	}
}

// Snapshot is everything a report or a status line needs, read at once so
// the numbers in it agree with each other.
type Snapshot struct {
	Session  Totals
	Day      Totals
	Month    Totals
	Total    Totals
	Models   map[string]Totals
	Sessions map[string]Totals
}

// Snapshot reads the ledger for one session.
func (l *Ledger) Snapshot(sessionKey string) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	s := Snapshot{
		Session:  l.book.Sessions[sessionKey].Totals,
		Day:      l.book.Days[dayKey(now)],
		Month:    l.book.Months[monthKey(now)],
		Total:    l.book.Total,
		Models:   make(map[string]Totals, len(l.book.Models)),
		Sessions: make(map[string]Totals, len(l.book.Sessions)),
	}
	for k, v := range l.book.Models {
		s.Models[k] = v
	}
	for k, v := range l.book.Sessions {
		s.Sessions[k] = v.Totals
	}
	return s
}

// Spent returns what one budget scope has cost so far.
func (s Snapshot) Spent(period string) Totals {
	switch period {
	case "day":
		return s.Day
	case "total":
		return s.Total
	}
	return s.Month
}
