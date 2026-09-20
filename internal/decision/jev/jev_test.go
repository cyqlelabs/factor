package jev

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

type capture struct {
	auth  string
	model string
	body  map[string]any
}

// fakeTypeSafe answers the System One endpoint the way the reference client
// expects it answered, recording what it was sent.
func fakeTypeSafe(t *testing.T, statuses []int, answers map[string]any) (*httptest.Server, *capture, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		cap.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &cap.body)
		cap.model, _ = cap.body["model"].(string)
		if n < len(statuses) && statuses[n] != 200 {
			w.WriteHeader(statuses[n])
			_, _ = w.Write([]byte(`{"error":"try later"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": answers, "model": "jev-1.13.0",
			"usage": map[string]any{"input_tokens": 512, "output_tokens": 8},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, cap, &calls
}

func testClient(url string) *Client {
	c := New("secret-key", url, "")
	c.sleep = func(time.Duration) {}
	return c
}

var oneQuestion = &decision.Request{
	State: map[string]any{"page": "x"},
	Questions: map[string]decision.Question{
		"op": {Criteria: map[string]any{"CLICK": "click", "DONE": "done"}, Instructions: map[string]any{"goal": "g"}},
	},
}

func TestDecideSendsTheReferenceWireFormatAndReadsTheReply(t *testing.T) {
	srv, cap, calls := fakeTypeSafe(t, nil, map[string]any{
		"op": map[string]any{"choice": "CLICK", "confidence": 0.9, "probabilities": map[string]any{"CLICK": 0.9, "DONE": 0.1}},
	})
	c := testClient(srv.URL)
	resp, err := c.Decide(context.Background(), oneQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || cap.auth != "Bearer secret-key" || cap.model != DefaultModel {
		t.Errorf("request: calls=%d auth=%q model=%q", calls.Load(), cap.auth, cap.model)
	}
	qs, _ := cap.body["questions"].(map[string]any)
	op, _ := qs["op"].(map[string]any)
	if op["criteria"] == nil || op["instructions"] == nil || cap.body["state"] == nil {
		t.Errorf("body missing state/criteria/instructions: %v", cap.body)
	}
	if resp.Model != "jev-1.13.0" || resp.Usage.InputTokens != 512 || resp.Usage.OutputTokens != 8 {
		t.Errorf("resp = %+v", resp)
	}
	if err := decision.Validate(resp.Answers["op"], []string{"CLICK", "DONE"}); err != nil {
		t.Errorf("answer did not validate: %v", err)
	}
	if c.Name() != "jev:"+DefaultModel || c.Model() != DefaultModel {
		t.Errorf("name=%q model=%q", c.Name(), c.Model())
	}
}

func TestDecideRetriesBackoffStatusesThenGivesUp(t *testing.T) {
	answers := map[string]any{"op": map[string]any{"choice": "DONE", "confidence": 0.7, "probabilities": map[string]any{"CLICK": 0.3, "DONE": 0.7}}}
	srv, _, calls := fakeTypeSafe(t, []int{429, 529, 200}, answers)
	if _, err := testClient(srv.URL).Decide(context.Background(), oneQuestion); err != nil {
		t.Fatalf("a request that recovered on the third try failed: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}

	srv2, _, calls2 := fakeTypeSafe(t, []int{503, 503, 503, 200}, answers)
	_, err := testClient(srv2.URL).Decide(context.Background(), oneQuestion)
	if !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if calls2.Load() != 3 {
		t.Errorf("calls = %d, want the bounded 3", calls2.Load())
	}
}

func TestDecideClassifiesFailures(t *testing.T) {
	srv, _, _ := fakeTypeSafe(t, []int{401}, nil)
	if _, err := testClient(srv.URL).Decide(context.Background(), oneQuestion); !errors.Is(err, decision.ErrInvalid) {
		t.Errorf("401: err = %v, want ErrInvalid", err)
	}
	srv5, _, _ := fakeTypeSafe(t, []int{500}, nil)
	if _, err := testClient(srv5.URL).Decide(context.Background(), oneQuestion); !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("500: err = %v, want ErrUnavailable", err)
	}
	dead := testClient("http://127.0.0.1:1")
	if _, err := dead.Decide(context.Background(), oneQuestion); !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("refused: err = %v, want ErrUnavailable", err)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json"))
	}))
	defer garbage.Close()
	if _, err := testClient(garbage.URL).Decide(context.Background(), oneQuestion); !errors.Is(err, decision.ErrInvalid) {
		t.Errorf("garbage: err = %v, want ErrInvalid", err)
	}
	noAnswers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev"}`))
	}))
	defer noAnswers.Close()
	if _, err := testClient(noAnswers.URL).Decide(context.Background(), oneQuestion); !errors.Is(err, decision.ErrInvalid) {
		t.Errorf("no answers: err = %v, want ErrInvalid", err)
	}
	inBodyError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"bad question"}}`))
	}))
	defer inBodyError.Close()
	if _, err := testClient(inBodyError.URL).Decide(context.Background(), oneQuestion); !errors.Is(err, decision.ErrInvalid) {
		t.Errorf("error body: err = %v, want ErrInvalid", err)
	}
}

func TestDecideHonoursTheContextDuringBackoff(t *testing.T) {
	srv, _, _ := fakeTypeSafe(t, []int{429, 429, 429}, nil)
	c := New("k", srv.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(time.Duration) { cancel() }
	_, err := c.Decide(ctx, oneQuestion)
	if !errors.Is(err, decision.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestUsageAcceptsBothSpellingsAndDefaultsTheModel(t *testing.T) {
	u := usageOf(map[string]any{"prompt_tokens": 10.0, "completion_tokens": 2.0})
	if u.InputTokens != 10 || u.OutputTokens != 2 {
		t.Errorf("usage = %+v", u)
	}
	if u := usageOf(nil); u.InputTokens != 0 {
		t.Errorf("nil usage = %+v", u)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"op":{"choice":"DONE","confidence":0.8,"probabilities":{"CLICK":0.2,"DONE":0.8}}}}`))
	}))
	defer srv.Close()
	resp, err := New("k", srv.URL+"/", "jev-custom").Decide(context.Background(), oneQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "jev-custom" {
		t.Errorf("model = %q, want the configured name when the reply names none", resp.Model)
	}
}

func TestCheckVerifiesTheEndpointLive(t *testing.T) {
	good, _, _ := fakeTypeSafe(t, nil, map[string]any{
		"color": map[string]any{"choice": "green", "confidence": 0.99, "probabilities": map[string]any{"green": 0.99, "red": 0.01}},
	})
	if err := Check(context.Background(), testClient(good.URL)); err != nil {
		t.Errorf("Check = %v", err)
	}
	wrongQuestion, _, _ := fakeTypeSafe(t, nil, map[string]any{"other": map[string]any{}})
	if err := Check(context.Background(), testClient(wrongQuestion.URL)); err == nil {
		t.Error("Check accepted a reply to a question it did not ask")
	}
	malformed, _, _ := fakeTypeSafe(t, nil, map[string]any{
		"color": map[string]any{"choice": "blue", "confidence": 0.99, "probabilities": map[string]any{"green": 0.99, "red": 0.01}},
	})
	if err := Check(context.Background(), testClient(malformed.URL)); !errors.Is(err, decision.ErrInvalid) {
		t.Errorf("Check on a malformed reply = %v", err)
	}
	down, _, _ := fakeTypeSafe(t, []int{500}, nil)
	if err := Check(context.Background(), testClient(down.URL)); err == nil {
		t.Error("Check passed a dead endpoint")
	}
}
