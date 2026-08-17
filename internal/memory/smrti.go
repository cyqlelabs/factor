package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Client talks to a smrti REST server. apiKey authenticates to smrti via
// X-Api-Key; extractKey rides in Authorization: Bearer so smrti can forward
// it to the LLM used for entity extraction (the middleware accepts either
// header for its own auth, by design).
type Client struct {
	baseURL    string
	apiKey     string
	extractKey string
	http       *http.Client
	healthy    atomic.Bool
	// Data calls in flight, and when the last one finished (unix nanos).
	// Restarting the engine is only safe in the gap between them, so the
	// upgrade path reads both through Idle.
	inflight   atomic.Int64
	lastActive atomic.Int64
	// routing is refreshed by every /status response: a `spaces` key is the
	// engine advertising per-request space routing. Old engines silently drop
	// unknown fields — the memory would land in the wrong space, not error —
	// so space fields are only ever sent once the engine has proved support.
	routing atomic.Pointer[spaceRouting]
}

// spaceRouting is what the last status probe said about spaces: whether the
// engine routes them, and the space it writes to when a request stays silent.
type spaceRouting struct {
	ok    bool
	space string
}

func NewClient(baseURL, apiKey, extractKey string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		extractKey: extractKey,
		http:       &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	if c.extractKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.extractKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.healthy.Store(false)
		return fmt.Errorf("smrti unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 {
			c.healthy.Store(false) // server up but failing; don't act on stale health
		}
		msg := string(data)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("smrti %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	c.healthy.Store(true)
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// activity marks one data request in flight; the returned call ends it. Only
// the calls that touch the graph are tracked: the supervisor probes /status
// every 30 seconds, so counting probes would leave the engine never idle and
// never upgradable.
func (c *Client) activity() func() {
	c.inflight.Add(1)
	return func() {
		c.lastActive.Store(time.Now().UnixNano())
		c.inflight.Add(-1)
	}
}

// Idle reports that nothing is reading or writing the graph right now and
// nothing has for quiet. Swapping the engine out from under a live request
// loses that memory, so the upgrade waits for this to be true.
func (c *Client) Idle(quiet time.Duration) bool {
	if c.inflight.Load() > 0 {
		return false
	}
	last := c.lastActive.Load()
	return last == 0 || time.Since(time.Unix(0, last)) >= quiet
}

func (c *Client) Remember(ctx context.Context, req RememberRequest) (string, error) {
	defer c.activity()()
	if req.Type == "" {
		req.Type = "episode"
	}
	if req.Probability == 0 {
		req.Probability = 0.8
	}
	var out struct {
		Status string `json:"status"`
		AtomID string `json:"atom_id"`
	}
	if req.Type == "belief" && req.Evidence != "" {
		body := map[string]any{"statement": req.Content, "probability": req.Probability, "evidence": req.Evidence}
		c.addSpace(body, req.Space)
		if err := c.do(ctx, http.MethodPost, "/believe", body, &out); err != nil {
			return "", err
		}
		return out.AtomID, nil
	}
	body := map[string]any{"content": req.Content, "type": req.Type, "probability": req.Probability}
	if req.Valence != nil {
		body["valence"] = *req.Valence
	}
	// Omitted when empty so the payload is byte-identical to what earlier
	// builds sent; older smrti versions ignore the field, newer ones default
	// it to "user", so either way an unset source behaves as before.
	if req.Source != "" {
		body["source"] = req.Source
	}
	c.addSpace(body, req.Space)
	if err := c.do(ctx, http.MethodPost, "/remember", body, &out); err != nil {
		return "", err
	}
	return out.AtomID, nil
}

// addSpace attaches a space to a request body — only when one is asked for
// and the engine has advertised space routing, so payloads to older engines
// stay byte-identical to pre-space builds.
func (c *Client) addSpace(body map[string]any, space string) {
	if ok, _ := c.SpaceSupport(); space != "" && ok {
		body["space"] = space
	}
}

// SpaceSupport reports what the last status probe said about space routing.
func (c *Client) SpaceSupport() (bool, string) {
	r := c.routing.Load()
	if r == nil {
		return false, ""
	}
	return r.ok, r.space
}

func (c *Client) Recall(ctx context.Context, query string, topK int, minConfidence float64, scope Scope) ([]Memory, error) {
	if query == "" {
		return nil, nil
	}
	defer c.activity()()
	if topK <= 0 {
		topK = 10
	}
	var out struct {
		Memories []Memory `json:"memories"`
	}
	body := map[string]any{"query": query, "top_k": topK, "min_confidence": minConfidence}
	c.addSpace(body, scope.Space)
	if ok, _ := c.SpaceSupport(); len(scope.ReadSpaces) > 0 && ok {
		body["read_spaces"] = scope.ReadSpaces
	}
	if err := c.do(ctx, http.MethodPost, "/recall", body, &out); err != nil {
		return nil, err
	}
	return out.Memories, nil
}

func (c *Client) Forget(ctx context.Context, query, reason, space string) error {
	defer c.activity()()
	body := map[string]any{"query": query}
	if reason != "" {
		body["reason"] = reason
	}
	c.addSpace(body, space)
	return c.do(ctx, http.MethodPost, "/forget", body, nil)
}

func (c *Client) Reflect(ctx context.Context) (map[string]any, error) {
	defer c.activity()()
	out := map[string]any{}
	if err := c.do(ctx, http.MethodPost, "/reflect", struct{}{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Status(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	if err := c.do(ctx, http.MethodGet, "/status", nil, &out); err != nil {
		return nil, err
	}
	_, ok := out["spaces"]
	space, _ := out["space"].(string)
	c.routing.Store(&spaceRouting{ok: ok, space: space})
	return out, nil
}

// CheckHealth probes /status with a short deadline and updates Healthy().
func (c *Client) CheckHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.Status(ctx)
	return err
}

func (c *Client) Enabled() bool { return true }
func (c *Client) Healthy() bool { return c.healthy.Load() }
func (c *Client) Close() error  { return nil }
