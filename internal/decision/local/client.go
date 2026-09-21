package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
)

// The wire client. It talks to a server on this machine and nowhere else, so
// there is no key to carry, no host to resolve and no hosted rate limit to
// respect — what is left is an HTTP POST and a strict reading of what comes
// back.
//
// The route is POST /v1/systemone, which is the contract TypeSafe's hosted
// model publishes and the one Laya's own reply already matches. Keeping it
// means the server on the other end can be swapped for any other that speaks
// it without touching this code.

const (
	// requestPath is the one route a decision travels.
	requestPath = "/v1/systemone"
	// maxBody bounds a reply. An answers map is kilobytes; past this it is
	// not one.
	maxBody = 1 << 20
	// loadingRetries is how many times a request is re-sent while the server
	// says it is still loading. The model takes seconds to build after a
	// restart, and a decision asked in that window is worth one more try
	// rather than an immediate fallback.
	loadingRetries = 2
	// retryPause is the wait between those tries.
	retryPause = 250 * time.Millisecond
)

// client posts decisions to the local server.
type client struct {
	base  string
	http  *http.Client
	sleep func(time.Duration)
}

func newClient(base string) *client {
	return &client{
		base:  strings.TrimRight(base, "/"),
		http:  &http.Client{Timeout: 30 * time.Second},
		sleep: time.Sleep,
	}
}

type wireRequest struct {
	Model     string                  `json:"model"`
	State     any                     `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

// wireQuestion is a question as the server wants it, which is not quite as
// Factor holds it: both `type` and `instructions` are required there, and a
// request missing either is refused before the model sees it. Factor's own
// callers all state their instructions, so the defaults below are a floor
// rather than a translation — what they prevent is a caller added later
// losing a decision to a 400 nobody reads.
type wireQuestion struct {
	Type         string         `json:"type"`
	Instructions any            `json:"instructions"`
	Criteria     map[string]any `json:"criteria"`
}

// questionType is the primitive a question asks for. Every question Factor
// asks is a choice over candidates the code enumerated — that is what the
// decision package is for — so the type is stated rather than inferred.
const questionType = "choice"

// defaultInstructions is what a question with none of its own says. It is
// deliberately plain: the criteria carry the meaning, and an invented
// instruction would be a second, quieter prompt nobody wrote.
const defaultInstructions = "Choose the candidate that best fits the state."

func wireQuestions(qs map[string]decision.Question) map[string]wireQuestion {
	out := make(map[string]wireQuestion, len(qs))
	for name, q := range qs {
		instructions := q.Instructions
		if instructions == nil {
			instructions = defaultInstructions
		}
		out[name] = wireQuestion{Type: questionType, Instructions: instructions, Criteria: q.Criteria}
	}
	return out
}

type wireResponse struct {
	Answers map[string]decision.Answer `json:"answers"`
	Model   string                     `json:"model"`
	Usage   map[string]any             `json:"usage"`
	Error   any                        `json:"error"`
}

// decide sends one request and reads the answers back.
//
// The two error classes matter to the caller and are kept apart: a transport
// failure or a server that is unwell is decision.ErrUnavailable, which the
// decider counts as a fallback and retries later; a request the server
// refused — a question whose options do not fit its head, a malformed body —
// is decision.ErrInvalid, because re-sending it would be refused again.
func (c *client) decide(ctx context.Context, req *decision.Request) (*decision.Response, error) {
	body, err := json.Marshal(wireRequest{Model: "laya", State: req.State, Questions: wireQuestions(req.Questions)})
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
		if status == http.StatusServiceUnavailable && attempt < loadingRetries {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %v", decision.ErrUnavailable, ctx.Err())
			default:
				c.sleep(retryPause)
			}
			continue
		}
		switch {
		case status >= 500 || status == http.StatusServiceUnavailable:
			return nil, fmt.Errorf("%w: HTTP %d: %s", decision.ErrUnavailable, status, firstLine(data))
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
		model = "laya"
	}
	return &decision.Response{
		Answers: wire.Answers,
		Model:   model,
		Usage: decision.Usage{
			InputTokens:  intOf(wire.Usage, "input_tokens", "prompt_tokens"),
			OutputTokens: intOf(wire.Usage, "output_tokens", "completion_tokens"),
		},
		Latency: time.Since(started),
	}, nil
}

func (c *client) post(ctx context.Context, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+requestPath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
