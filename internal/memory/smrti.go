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

func (c *Client) Remember(ctx context.Context, req RememberRequest) (string, error) {
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
		if err := c.do(ctx, http.MethodPost, "/believe", body, &out); err != nil {
			return "", err
		}
		return out.AtomID, nil
	}
	body := map[string]any{"content": req.Content, "type": req.Type, "probability": req.Probability}
	if req.Valence != nil {
		body["valence"] = *req.Valence
	}
	if err := c.do(ctx, http.MethodPost, "/remember", body, &out); err != nil {
		return "", err
	}
	return out.AtomID, nil
}

func (c *Client) Recall(ctx context.Context, query string, topK int, minConfidence float64) ([]Memory, error) {
	if query == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = 10
	}
	var out struct {
		Memories []Memory `json:"memories"`
	}
	body := map[string]any{"query": query, "top_k": topK, "min_confidence": minConfidence}
	if err := c.do(ctx, http.MethodPost, "/recall", body, &out); err != nil {
		return nil, err
	}
	return out.Memories, nil
}

func (c *Client) Forget(ctx context.Context, query, reason string) error {
	body := map[string]any{"query": query}
	if reason != "" {
		body["reason"] = reason
	}
	return c.do(ctx, http.MethodPost, "/forget", body, nil)
}

func (c *Client) Reflect(ctx context.Context) (map[string]any, error) {
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
	return out, nil
}

// CheckHealth probes /status with a short deadline and updates Healthy().
func (c *Client) CheckHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.Status(ctx)
	return err
}

func (c *Client) Healthy() bool { return c.healthy.Load() }
func (c *Client) Close() error  { return nil }
