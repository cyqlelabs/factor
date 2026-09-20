// Package jev is the HTTP client for TypeSafe's System One endpoint, which
// answers typed questions with a choice, a probability per candidate and a
// confidence. It is a direct client rather than a sidecar: the wire format is
// one JSON document each way, and there is nothing a Python process would add
// but a second thing to supervise.
//
// The shape of the request and the reply is the one the reference agent
// (browser-use/jev-ultrafast, model.py) sends and validates: a model name,
// a state object, a map of questions each carrying criteria and instructions,
// and an answers map keyed by question name. Nothing here is trusted on the
// way back — decision.Validate refuses anything malformed before a caller
// can act on it.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
)

const (
	// DefaultBase is TypeSafe's API host.
	DefaultBase = "https://api.typesafe.ai"
	// DefaultModel is the alias TypeSafe resolves to the current Jev release.
	DefaultModel = "jev-latest"
	// path is the System One endpoint.
	path = "/v1/systemone"
	// retries bounds how many times a request is re-sent after the backend
	// asks it to back off. A decision is worth having only while the caller
	// is still waiting, so two retries is already generous.
	retries = 2
	// maxBody bounds a reply. An answers map is a few kilobytes; anything
	// past this is not one.
	maxBody = 1 << 20
)

// Client speaks to one System One endpoint.
type Client struct {
	apiKey string
	base   string
	model  string
	http   *http.Client
	// sleep is how the client waits between retries; tests shrink it.
	sleep func(time.Duration)
}

// New builds a client. base and model take the defaults when blank. The
// HTTP client rides http.DefaultTransport rather than a transport of its
// own, so the proxy and the CA the process was started with reach it too.
func New(apiKey, base, model string) *Client {
	if base == "" {
		base = DefaultBase
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		apiKey: apiKey,
		base:   strings.TrimRight(base, "/"),
		model:  model,
		http:   &http.Client{Timeout: 30 * time.Second},
		sleep:  time.Sleep,
	}
}

// Name implements decision.Backend.
func (c *Client) Name() string { return "jev:" + c.model }

// Model is the model name requests carry.
func (c *Client) Model() string { return c.model }

type wireRequest struct {
	Model     string                       `json:"model"`
	State     any                          `json:"state"`
	Questions map[string]decision.Question `json:"questions"`
}

type wireResponse struct {
	Answers map[string]decision.Answer `json:"answers"`
	Model   string                     `json:"model"`
	Usage   map[string]any             `json:"usage"`
	Error   any                        `json:"error"`
}

// Decide implements decision.Backend. Transport failures and 5xx replies are
// decision.ErrUnavailable; a 4xx other than a back-off, and a body that does
// not parse, are decision.ErrInvalid — the request was wrong, or the reply
// was, and re-sending it will not help.
func (c *Client) Decide(ctx context.Context, req *decision.Request) (*decision.Response, error) {
	body, err := json.Marshal(wireRequest{Model: c.model, State: req.State, Questions: req.Questions})
	if err != nil {
		return nil, fmt.Errorf("%w: encode request: %v", decision.ErrInvalid, err)
	}
	started := time.Now()
	var raw []byte
	for attempt := 0; ; attempt++ {
		status, data, err := c.post(ctx, body)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", decision.ErrUnavailable, err)
		}
		if backoff(status) && attempt < retries {
			wait := 500 * time.Millisecond << attempt
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %v", decision.ErrUnavailable, ctx.Err())
			default:
				c.sleep(wait)
			}
			continue
		}
		switch {
		case status >= 500 || backoff(status):
			return nil, fmt.Errorf("%w: HTTP %d", decision.ErrUnavailable, status)
		case status >= 400:
			return nil, fmt.Errorf("%w: HTTP %d: %s", decision.ErrInvalid, status, firstLine(data))
		}
		raw = data
		break
	}
	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("%w: decode reply: %v", decision.ErrInvalid, err)
	}
	if wire.Error != nil {
		return nil, fmt.Errorf("%w: %v", decision.ErrInvalid, wire.Error)
	}
	if wire.Answers == nil {
		return nil, fmt.Errorf("%w: reply carries no answers", decision.ErrInvalid)
	}
	model := wire.Model
	if model == "" {
		model = c.model
	}
	return &decision.Response{
		Answers: wire.Answers,
		Model:   model,
		Usage:   usageOf(wire.Usage),
		Latency: time.Since(started),
	}, nil
}

func (c *Client) post(ctx context.Context, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// backoff names the statuses the reference client retries: rate limited,
// overloaded, and the 529 some gateways answer with under load.
func backoff(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == 529
}

// usageOf reads token counts out of whichever spelling the endpoint uses.
// The OpenAI names are the ones seen on the wire; the Anthropic names are
// accepted so a gateway that translates does not zero the bill.
func usageOf(u map[string]any) decision.Usage {
	return decision.Usage{
		InputTokens:  intOf(u, "input_tokens", "prompt_tokens"),
		OutputTokens: intOf(u, "output_tokens", "completion_tokens"),
	}
}

func intOf(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok {
			return int(f)
		}
	}
	return 0
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// Check sends one minimal question so a configured key and endpoint can be
// verified live, the way every other wizard step verifies itself. It costs
// a few dozen input tokens.
func Check(ctx context.Context, c *Client) error {
	resp, err := c.Decide(ctx, &decision.Request{
		State: map[string]any{"text": "The light is green."},
		Questions: map[string]decision.Question{
			"color": {Criteria: map[string]any{"green": "the light is green", "red": "the light is red"}},
		},
	})
	if err != nil {
		return err
	}
	a, ok := resp.Answers["color"]
	if !ok {
		return errors.New("the endpoint answered without the question asked")
	}
	return decision.Validate(a, []string{"green", "red"})
}
