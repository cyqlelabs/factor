package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

type Reason string

const (
	ReasonAuth            Reason = "auth"
	ReasonBilling         Reason = "billing"
	ReasonRateLimit       Reason = "rate_limit"
	ReasonNetwork         Reason = "network"
	ReasonTimeout         Reason = "timeout"
	ReasonOverloaded      Reason = "overloaded"
	ReasonContextOverflow Reason = "context_overflow"
	ReasonFormat          Reason = "format"
	ReasonUnknown         Reason = "unknown"
)

// APIError is a classified provider failure.
type APIError struct {
	Provider string
	Status   int
	Body     string
	Reason   Reason
	Err      error
}

func (e *APIError) Error() string {
	msg := e.Body
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Provider, e.Reason, e.Err)
	}
	return fmt.Sprintf("%s: %s (HTTP %d): %s", e.Provider, e.Reason, e.Status, msg)
}

func (e *APIError) Unwrap() error { return e.Err }

// Retriable reports whether another attempt (same or different candidate)
// could plausibly succeed.
func (e *APIError) Retriable() bool {
	switch e.Reason {
	case ReasonRateLimit, ReasonNetwork, ReasonTimeout, ReasonOverloaded, ReasonUnknown:
		return true
	}
	return false
}

var contextOverflowMarkers = []string{
	"context length", "context_length_exceeded", "maximum context",
	"too many tokens", "prompt is too long", "input is too long",
	"context window", "max_tokens_to_sample",
}

// ClassifyTransport classifies a transport-level (pre-HTTP-status) error.
func ClassifyTransport(providerName string, err error) *APIError {
	reason := ReasonNetwork
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = ReasonTimeout
	case errors.As(err, &netErr) && netErr.Timeout():
		reason = ReasonTimeout
	}
	return &APIError{Provider: providerName, Reason: reason, Err: err}
}

// ClassifyStatus classifies a non-2xx HTTP response.
func ClassifyStatus(providerName string, status int, body string) *APIError {
	e := &APIError{Provider: providerName, Status: status, Body: body, Reason: ReasonUnknown}
	lower := strings.ToLower(body)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(lower, marker) {
			e.Reason = ReasonContextOverflow
			return e
		}
	}
	switch {
	case status == 401 || status == 403:
		e.Reason = ReasonAuth
	case status == 402:
		e.Reason = ReasonBilling
	case status == 429:
		e.Reason = ReasonRateLimit
	case status == 408:
		e.Reason = ReasonTimeout
	case status == 400 || status == 404 || status == 422:
		e.Reason = ReasonFormat
	case status == 529 || status == 503 || status == 502:
		e.Reason = ReasonOverloaded
	case status >= 500:
		e.Reason = ReasonUnknown
	}
	return e
}
