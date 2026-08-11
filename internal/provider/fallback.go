package provider

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Chain tries candidates in order, applying per-candidate cooldowns so a
// failing provider (bad key, rate limit, outage) is skipped while it cools
// down instead of stalling every turn. Context-overflow aborts immediately so
// the caller can compact and retry.
type Chain struct {
	providers  []Provider
	maxRetries int
	backoff    time.Duration

	mu       sync.Mutex
	cooldown map[string]time.Time
	fails    map[string]int

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func NewChain(providers []Provider, maxRetries int, backoff time.Duration) *Chain {
	if maxRetries < 0 {
		maxRetries = 0
	}
	return &Chain{
		providers:  providers,
		maxRetries: maxRetries,
		backoff:    backoff,
		cooldown:   map[string]time.Time{},
		fails:      map[string]int{},
		now:        time.Now,
		sleep:      sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Primary returns the first candidate (used for auxiliary calls like summarization).
func (c *Chain) Primary() Provider {
	if len(c.providers) == 0 {
		return nil
	}
	return c.providers[0]
}

const (
	hardFailCooldown = 10 * time.Minute
	maxSoftCooldown  = 5 * time.Minute
)

func (c *Chain) softCooldown(name string) time.Duration {
	c.fails[name]++
	d := c.backoff * time.Duration(1<<uint(min(c.fails[name]-1, 6)))
	if d > maxSoftCooldown {
		d = maxSoftCooldown
	}
	return d
}

// Chat runs the request through the chain.
func (c *Chain) Chat(ctx context.Context, req *Request) (*Response, error) {
	if len(c.providers) == 0 {
		return nil, errors.New("no providers configured")
	}
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, c.backoff*time.Duration(attempt)); err != nil {
				return nil, err
			}
		}
		resp, err, aborted := c.tryAll(ctx, req, attempt == c.maxRetries)
		if errors.Is(err, errAllCoolingDown) && !aborted {
			// Every candidate was skipped; sleeping without trying anything
			// is pure waste, so retry immediately ignoring cooldowns.
			resp, err, aborted = c.tryAll(ctx, req, true)
		}
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if aborted {
			break
		}
	}
	return nil, lastErr
}

// tryAll walks every candidate once. On the final attempt, cooldowns are
// ignored so the chain can never deadlock itself.
func (c *Chain) tryAll(ctx context.Context, req *Request, ignoreCooldown bool) (resp *Response, lastErr error, aborted bool) {
	for _, p := range c.providers {
		if ctx.Err() != nil {
			return nil, ctx.Err(), true
		}
		name := p.Name()
		c.mu.Lock()
		until, cooling := c.cooldown[name]
		c.mu.Unlock()
		if cooling && c.now().Before(until) && !ignoreCooldown {
			continue
		}

		resp, err := p.Chat(ctx, req)
		if err == nil {
			c.mu.Lock()
			delete(c.cooldown, name)
			delete(c.fails, name)
			c.mu.Unlock()
			return resp, nil, false
		}
		lastErr = err
		if errors.Is(err, context.Canceled) {
			return nil, err, true
		}

		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.Reason == ReasonContextOverflow {
				return nil, err, true // caller must compact; retrying is futile
			}
			c.mu.Lock()
			if apiErr.Retriable() {
				c.cooldown[name] = c.now().Add(c.softCooldown(name))
			} else {
				c.cooldown[name] = c.now().Add(hardFailCooldown)
			}
			c.mu.Unlock()
			slog.Warn("provider failed, trying next candidate", "provider", name, "reason", apiErr.Reason)
			continue
		}
		return nil, err, true // unclassified programming error: do not mask it
	}
	if lastErr == nil {
		lastErr = errAllCoolingDown
	}
	return nil, lastErr, false
}

var errAllCoolingDown = errors.New("all providers cooling down")

// IsContextOverflow reports whether err is a classified context-overflow failure.
func IsContextOverflow(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Reason == ReasonContextOverflow
}
