package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/decision"
)

// The client is the one place a decision crosses a process boundary, so what
// it does with each kind of answer is what decides whether a caller falls
// back, retries, or acts.

var oneQuestion = &decision.Request{
	State: map[string]any{"page": "x"},
	Questions: map[string]decision.Question{
		"op": {Criteria: map[string]any{"CLICK": "click", "DONE": "done"}, Instructions: map[string]any{"goal": "g"}},
	},
}

type capture struct {
	body map[string]any
	auth string
}

// fakeServer answers /v1/systemone with the given statuses in order, then
// with a well-formed reply.
func fakeServer(t *testing.T, statuses []int, answers map[string]any) (*httptest.Server, *capture, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if r.URL.Path != requestPath || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		cap.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &cap.body)
		if n < len(statuses) && statuses[n] != 200 {
			w.WriteHeader(statuses[n])
			_, _ = w.Write([]byte(`{"error":"not yet"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": answers, "model": "laya-multilingual",
			"usage": map[string]any{"input_tokens": 512, "output_tokens": 0},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, cap, &calls
}

func testClient(url string) *client {
	c := newClient(url)
	c.sleep = func(time.Duration) {}
	return c
}

var goodAnswer = map[string]any{
	"op": map[string]any{"choice": "CLICK", "confidence": 0.9, "probabilities": map[string]any{"CLICK": 0.9, "DONE": 0.1}},
}

func TestClientSendsTheContractAndReadsTheReply(t *testing.T) {
	srv, cap, calls := fakeServer(t, nil, goodAnswer)
	resp, err := testClient(srv.URL).decide(context.Background(), oneQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d", calls.Load())
	}
	// Nothing authenticates a request to this machine.
	if cap.auth != "" {
		t.Errorf("the client sent credentials to a local server: %q", cap.auth)
	}
	qs, _ := cap.body["questions"].(map[string]any)
	op, _ := qs["op"].(map[string]any)
	if op["criteria"] == nil || op["instructions"] == nil || cap.body["state"] == nil {
		t.Errorf("body missing state/criteria/instructions: %v", cap.body)
	}
	if resp.Model != "laya-multilingual" || resp.Usage.InputTokens != 512 || resp.Latency <= 0 {
		t.Errorf("resp = %+v", resp)
	}
	if err := decision.Validate(resp.Answers["op"], []string{"CLICK", "DONE"}); err != nil {
		t.Errorf("answer did not validate: %v", err)
	}
}

// A model still building its checkpoint answers 503, and a decision asked in
// that window is worth one more try rather than an immediate fallback.
func TestClientWaitsOutAModelThatIsStillLoading(t *testing.T) {
	srv, _, calls := fakeServer(t, []int{503, 503, 200}, goodAnswer)
	if _, err := testClient(srv.URL).decide(context.Background(), oneQuestion); err != nil {
		t.Fatalf("a request that recovered on the third try failed: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}

	// But not forever: a model that never comes up is a fallback.
	srv2, _, calls2 := fakeServer(t, []int{503, 503, 503, 200}, goodAnswer)
	_, err := testClient(srv2.URL).decide(context.Background(), oneQuestion)
	if !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if calls2.Load() != 3 {
		t.Errorf("calls = %d, want the bounded 3", calls2.Load())
	}
}

// The two error classes are kept apart because the caller treats them
// differently: one is retried later, the other would be refused again.
func TestClientClassifiesFailures(t *testing.T) {
	cases := map[string]struct {
		handler http.HandlerFunc
		want    error
	}{
		"a refused request": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"question 'click_target' options exceed head_max_len=192"}`))
		}, decision.ErrInvalid},
		"a server that broke": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(500)
		}, decision.ErrUnavailable},
		"not JSON": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>no"))
		}, decision.ErrInvalid},
		"no answers": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":"laya"}`))
		}, decision.ErrInvalid},
		"an error in the body": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"error":{"message":"bad question"}}`))
		}, decision.ErrInvalid},
	}
	for name, c := range cases {
		srv := httptest.NewServer(c.handler)
		_, err := testClient(srv.URL).decide(context.Background(), oneQuestion)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
		srv.Close()
	}
	// Nothing listening at all.
	if _, err := testClient("http://127.0.0.1:1").decide(context.Background(), oneQuestion); !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("refused: err = %v, want ErrUnavailable", err)
	}
}

func TestClientHonoursTheContextWhileWaiting(t *testing.T) {
	srv, _, _ := fakeServer(t, []int{503, 503, 503}, nil)
	c := newClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(time.Duration) { cancel() }
	if _, err := c.decide(ctx, oneQuestion); !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestUsageAcceptsBothSpellings(t *testing.T) {
	if u := intOf(map[string]any{"prompt_tokens": 10.0}, "input_tokens", "prompt_tokens"); u != 10 {
		t.Errorf("usage = %d", u)
	}
	if u := intOf(nil, "input_tokens"); u != 0 {
		t.Errorf("nil usage = %d", u)
	}
	// A reply that names no model is still this one.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"op":{"choice":"DONE","confidence":0.8,"probabilities":{"CLICK":0.2,"DONE":0.8}}}}`))
	}))
	defer srv.Close()
	resp, err := newClient(srv.URL+"/").decide(context.Background(), oneQuestion)
	if err != nil || resp.Model != "laya" {
		t.Errorf("resp = %+v err = %v", resp, err)
	}
}
