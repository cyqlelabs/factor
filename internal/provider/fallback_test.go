package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChainPrimaryReturnsFirstProvider(t *testing.T) {
	first := &scriptedProvider{name: "first"}
	second := &scriptedProvider{name: "second"}
	chain := NewChain([]Provider{first, second}, 1, time.Second)
	if got := chain.Primary(); got != Provider(first) {
		t.Errorf("Primary() = %v, want the first provider", got)
	}
}

func TestChainPrimaryIsNilWhenEmpty(t *testing.T) {
	if got := NewChain(nil, 1, time.Second).Primary(); got != nil {
		t.Errorf("Primary() = %v, want nil for an empty chain", got)
	}
}

func TestChainChatWithoutProvidersFails(t *testing.T) {
	_, err := NewChain(nil, 2, time.Second).Chat(context.Background(), &Request{})
	if err == nil {
		t.Fatal("want an error when no providers are configured")
	}
	if !strings.Contains(err.Error(), "no providers configured") {
		t.Errorf("err = %v", err)
	}
}

func TestChainAbortsOnAlreadyCancelledContext(t *testing.T) {
	p := &scriptedProvider{name: "p", fn: func(int) (*Response, error) {
		return &Response{Content: "should not be reached"}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fastChain(p).Chat(ctx, &Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if p.calls != 0 {
		t.Errorf("provider called %d times on a cancelled context", p.calls)
	}
}

func TestChainAbortsWhenProviderReportsCancellation(t *testing.T) {
	cancelled := &scriptedProvider{name: "cancelled", fn: func(int) (*Response, error) {
		return nil, context.Canceled
	}}
	next := &scriptedProvider{name: "next", fn: func(int) (*Response, error) {
		return &Response{Content: "should not be reached"}, nil
	}}
	_, err := fastChain(cancelled, next).Chat(context.Background(), &Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if next.calls != 0 {
		t.Error("cancellation must abort the chain, not fail over")
	}
	if cancelled.calls != 1 {
		t.Errorf("cancelled provider called %d times, want 1 (no retry)", cancelled.calls)
	}
}

func TestChainReturnsSleepError(t *testing.T) {
	stop := errors.New("sleep interrupted")
	failing := &scriptedProvider{name: "failing", fn: func(int) (*Response, error) {
		return nil, &APIError{Provider: "failing", Reason: ReasonOverloaded}
	}}
	chain := NewChain([]Provider{failing}, 2, time.Millisecond)
	chain.sleep = func(context.Context, time.Duration) error { return stop }

	_, err := chain.Chat(context.Background(), &Request{})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the sleep error", err)
	}
	if failing.calls != 1 {
		t.Errorf("provider called %d times, want 1 before the interrupted backoff", failing.calls)
	}
}

func TestChainRetriesImmediatelyWhenEveryCandidateIsCoolingDown(t *testing.T) {
	// Time is frozen, so the cooldown set on attempt 0 is still active on
	// attempt 1: the chain must not burn that attempt doing nothing.
	flaky := &scriptedProvider{name: "flaky", fn: func(call int) (*Response, error) {
		if call <= 2 {
			return nil, &APIError{Provider: "flaky", Reason: ReasonOverloaded}
		}
		return &Response{Content: "recovered"}, nil
	}}
	chain := NewChain([]Provider{flaky}, 2, time.Minute)
	chain.sleep = func(context.Context, time.Duration) error { return nil }
	frozen := time.Unix(1_700_000_000, 0)
	chain.now = func() time.Time { return frozen }

	resp, err := chain.Chat(context.Background(), &Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "recovered" {
		t.Errorf("content = %q", resp.Content)
	}
	if flaky.calls != 3 {
		t.Errorf("provider called %d times, want 3 (the cooling-down attempt retries at once)", flaky.calls)
	}
}

func TestChainClearsCooldownAfterSuccess(t *testing.T) {
	call := 0
	flaky := &scriptedProvider{name: "flaky", fn: func(int) (*Response, error) {
		call++
		if call == 1 {
			return nil, &APIError{Provider: "flaky", Reason: ReasonRateLimit}
		}
		return &Response{Content: "ok"}, nil
	}}
	chain := NewChain([]Provider{flaky}, 2, time.Minute)
	chain.sleep = func(context.Context, time.Duration) error { return nil }
	frozen := time.Unix(1_700_000_000, 0)
	chain.now = func() time.Time { return frozen }

	if _, err := chain.Chat(context.Background(), &Request{}); err != nil {
		t.Fatal(err)
	}
	chain.mu.Lock()
	_, cooling := chain.cooldown["flaky"]
	fails := chain.fails["flaky"]
	chain.mu.Unlock()
	if cooling {
		t.Error("a successful call must clear the cooldown")
	}
	if fails != 0 {
		t.Errorf("fail count = %d, want it reset after success", fails)
	}
}

func TestChainNegativeMaxRetriesMeansSingleAttempt(t *testing.T) {
	failing := &scriptedProvider{name: "failing", fn: func(int) (*Response, error) {
		return nil, &APIError{Provider: "failing", Reason: ReasonOverloaded}
	}}
	chain := NewChain([]Provider{failing}, -5, time.Millisecond)
	chain.sleep = func(context.Context, time.Duration) error {
		t.Error("no backoff sleep should happen with a single attempt")
		return nil
	}
	if _, err := chain.Chat(context.Background(), &Request{}); err == nil {
		t.Fatal("want the provider error")
	}
	if failing.calls != 1 {
		t.Errorf("provider called %d times, want 1", failing.calls)
	}
}

func TestSoftCooldownGrowsExponentiallyAndIsCapped(t *testing.T) {
	chain := NewChain(nil, 0, time.Minute)
	if d := chain.softCooldown("p"); d != time.Minute {
		t.Errorf("first cooldown = %v, want the base backoff", d)
	}
	if d := chain.softCooldown("p"); d != 2*time.Minute {
		t.Errorf("second cooldown = %v, want double the base backoff", d)
	}
	var last time.Duration
	for range 10 {
		last = chain.softCooldown("p")
	}
	if last != maxSoftCooldown {
		t.Errorf("cooldown = %v, want it capped at %v", last, maxSoftCooldown)
	}
}

func TestSleepCtxWaitsAndHonoursCancellation(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepCtx = %v, want nil after the timer fires", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepCtx = %v, want context.Canceled without waiting", err)
	}
}
