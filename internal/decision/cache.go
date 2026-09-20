package decision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// A decision is very nearly a pure function of the model, the questions and
// the state, which means it is worth remembering. The idea is jevcache's;
// this is the small in-process version of it, because what Factor needs is a
// memo and not a ledger — a second binary to supervise costs more than it
// would save here.
//
// The repeats are real and they are the expensive kind. The browser executor
// re-observes after every action and asks the same question of a page an
// action did not change; a stall repeats a call and the recovery question
// with it; a turn that overflows and recompacts re-verifies a reply it
// already verified. Each of those is a round trip the user waits through and,
// on the hosted backend, a bill. A hit costs a map lookup.
//
// Two things keep it honest. The key is over the request as it would go on
// the wire, canonically encoded, so two requests share an entry only when
// they are the same request. And a hit carries no usage: the tokens were
// billed the first time, and counting them again would report spend that
// never happened.

// Cache memoizes a backend's answers, bounded, with the oldest entry evicted
// first. It is safe for concurrent use.
type Cache struct {
	inner Backend
	limit int

	mu      sync.Mutex
	entries map[string]*Response
	order   []string

	hits, misses int
}

// Cached wraps a backend in a memo of at most limit answers. A limit of zero
// or less returns the backend unwrapped, which is how the feature is turned
// off.
func Cached(inner Backend, limit int) Backend {
	if inner == nil || limit <= 0 {
		return inner
	}
	return &Cache{inner: inner, limit: limit, entries: map[string]*Response{}}
}

// Name implements Backend, naming what is behind the cache rather than the
// cache: a fallback reason that said "cache" would name the wrong thing.
func (c *Cache) Name() string { return c.inner.Name() }

// Limits implements Limited by passing the wrapped backend's through, so a
// cache in front of the local model does not hide its window.
func (c *Cache) Limits() Limits {
	if limited, ok := c.inner.(Limited); ok {
		return limited.Limits()
	}
	return Limits{}
}

// Decide answers from the memo when it can, and remembers what it had to ask
// for. Only a successful answer is remembered: an error is a fact about the
// backend at a moment, not about the question.
func (c *Cache) Decide(ctx context.Context, req *Request) (*Response, error) {
	key, ok := fingerprint(c.inner.Name(), req)
	if !ok {
		return c.inner.Decide(ctx, req)
	}
	if resp := c.get(key); resp != nil {
		return resp, nil
	}
	resp, err := c.inner.Decide(ctx, req)
	if err != nil {
		return nil, err
	}
	c.put(key, resp)
	return resp, nil
}

// Stats reports how the memo has done, for the log and the tests.
func (c *Cache) Stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

func (c *Cache) get(key string) *Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, ok := c.entries[key]
	if !ok {
		c.misses++
		return nil
	}
	c.hits++
	// A copy, with the usage cleared: the caller may bill what it is handed,
	// and these tokens were paid for once already. Answers are never mutated
	// by a caller, so the map may keep sharing them.
	hit := *resp
	hit.Usage = Usage{}
	hit.billed = true
	return &hit
}

func (c *Cache) put(key string, resp *Response) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	if len(c.order) >= c.limit {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	c.entries[key] = resp
	c.order = append(c.order, key)
}

// fingerprint identifies a request by what it would send: the backend that
// would answer it, the state, and every question with its criteria and
// instructions. json.Marshal sorts map keys, so two requests that differ only
// in map iteration order hash the same. A request that cannot be encoded is
// not cached rather than being given a key that collides with another.
func fingerprint(backend string, req *Request) (string, bool) {
	raw, err := json.Marshal(struct {
		Backend   string              `json:"b"`
		State     any                 `json:"s"`
		Questions map[string]Question `json:"q"`
	}{backend, req.State, req.Questions})
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true
}
