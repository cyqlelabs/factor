//go:build tck

package a2a

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestServeForTCK serves this package's server on :9999 for the A2A
// conformance kit (github.com/a2aproject/a2a-tck), with every caller taken as
// one agent and every task answered after a moment's work:
//
//	go test -tags tck -run TestServeForTCK -timeout 0 ./internal/a2a
//	./run_tck.py --sut-host http://localhost:9999 --transport jsonrpc --level must
//
// It runs until FACTOR_TCK_SECONDS have passed (default ten minutes).
func TestServeForTCK(t *testing.T) {
	store, err := OpenStore(t.TempDir(), KeepTasks)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(runnerFunc(func(ctx context.Context, turn Turn) (string, error) {
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "done: " + turn.Text, nil
	}), store, Options{Self: "sut", Concurrent: 16, Queue: 64,
		Card: Card("Factor TCK", "1.0.0", "http://localhost:9999")})
	defer srv.Close()
	hs := &http.Server{Addr: "localhost:9999", Handler: srv.Handler(func(*http.Request) (string, bool) { return "tck", true })}
	go func() { _ = hs.ListenAndServe() }()
	defer hs.Close()
	limit := 10 * time.Minute
	if v, err := time.ParseDuration(os.Getenv("FACTOR_TCK_SECONDS") + "s"); err == nil {
		limit = v
	}
	time.Sleep(limit)
}
