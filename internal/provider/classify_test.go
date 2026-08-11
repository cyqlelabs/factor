package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

// fakeNetError implements net.Error with a configurable Timeout answer.
type fakeNetError struct{ timeout bool }

func (e fakeNetError) Error() string   { return "fake net error" }
func (e fakeNetError) Timeout() bool   { return e.timeout }
func (e fakeNetError) Temporary() bool { return false }

func TestClassifyTransportDeadlineExceededIsTimeout(t *testing.T) {
	e := ClassifyTransport("p", context.DeadlineExceeded)
	if e.Reason != ReasonTimeout {
		t.Errorf("reason = %s, want %s", e.Reason, ReasonTimeout)
	}
	if !e.Retriable() {
		t.Error("a timeout must be retriable")
	}
}

func TestClassifyTransportUnwrapsWrappedDeadline(t *testing.T) {
	wrapped := fmt.Errorf("dialing upstream: %w", context.DeadlineExceeded)
	e := ClassifyTransport("p", wrapped)
	if e.Reason != ReasonTimeout {
		t.Errorf("reason = %s, want %s for a wrapped deadline", e.Reason, ReasonTimeout)
	}
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Error("classified error must still unwrap to the cause")
	}
}

func TestClassifyTransportNetTimeoutIsTimeout(t *testing.T) {
	var netErr net.Error = fakeNetError{timeout: true}
	e := ClassifyTransport("p", netErr)
	if e.Reason != ReasonTimeout {
		t.Errorf("reason = %s, want %s", e.Reason, ReasonTimeout)
	}
}

func TestClassifyTransportNonTimeoutNetErrorIsNetwork(t *testing.T) {
	var netErr net.Error = fakeNetError{timeout: false}
	e := ClassifyTransport("p", netErr)
	if e.Reason != ReasonNetwork {
		t.Errorf("reason = %s, want %s", e.Reason, ReasonNetwork)
	}
}

func TestClassifyTransportGenericErrorIsNetwork(t *testing.T) {
	cause := errors.New("connection reset by peer")
	e := ClassifyTransport("openai:m", cause)
	if e.Reason != ReasonNetwork {
		t.Errorf("reason = %s, want %s", e.Reason, ReasonNetwork)
	}
	if e.Provider != "openai:m" {
		t.Errorf("provider = %q", e.Provider)
	}
	if !errors.Is(e, cause) {
		t.Error("cause must remain reachable through errors.Is")
	}
	if !e.Retriable() {
		t.Error("a network error must be retriable")
	}
}

func TestClassifyStatusRemainingCodes(t *testing.T) {
	cases := []struct {
		status int
		want   Reason
	}{
		{408, ReasonTimeout},
		{404, ReasonFormat},
		{422, ReasonFormat},
		{502, ReasonOverloaded},
		{599, ReasonUnknown},
		{200, ReasonUnknown},
	}
	for _, c := range cases {
		if got := ClassifyStatus("p", c.status, "").Reason; got != c.want {
			t.Errorf("status %d → %s, want %s", c.status, got, c.want)
		}
	}
}

func TestAPIErrorMessageWithWrappedCause(t *testing.T) {
	e := &APIError{Provider: "openai:gpt", Reason: ReasonFormat, Status: 200, Body: "ignored", Err: errors.New("boom")}
	if got, want := e.Error(), "openai:gpt: format: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestAPIErrorMessageWithoutCauseShowsStatusAndBody(t *testing.T) {
	e := &APIError{Provider: "anthropic:c", Reason: ReasonAuth, Status: 401, Body: "invalid key"}
	if got, want := e.Error(), "anthropic:c: auth (HTTP 401): invalid key"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestAPIErrorTruncatesLongBody(t *testing.T) {
	body := strings.Repeat("x", 300) + "SECRET-TAIL"
	e := &APIError{Provider: "p", Reason: ReasonUnknown, Status: 500, Body: body}
	msg := e.Error()
	if strings.Contains(msg, "SECRET-TAIL") {
		t.Error("body past 300 chars must be dropped")
	}
	if !strings.HasSuffix(msg, strings.Repeat("x", 300)) {
		t.Errorf("want the first 300 body chars kept, got %q", msg)
	}
}

func TestAPIErrorUnwrap(t *testing.T) {
	cause := errors.New("root cause")
	e := &APIError{Provider: "p", Reason: ReasonNetwork, Err: cause}
	if errors.Unwrap(e) != cause {
		t.Errorf("Unwrap() = %v, want %v", errors.Unwrap(e), cause)
	}
	if errors.Unwrap(&APIError{Provider: "p", Reason: ReasonAuth}) != nil {
		t.Error("Unwrap() must be nil when there is no cause")
	}
}

func TestAPIErrorRetriableByReason(t *testing.T) {
	cases := map[Reason]bool{
		ReasonRateLimit:       true,
		ReasonNetwork:         true,
		ReasonTimeout:         true,
		ReasonOverloaded:      true,
		ReasonUnknown:         true,
		ReasonAuth:            false,
		ReasonBilling:         false,
		ReasonFormat:          false,
		ReasonContextOverflow: false,
	}
	for reason, want := range cases {
		if got := (&APIError{Reason: reason}).Retriable(); got != want {
			t.Errorf("%s retriable = %v, want %v", reason, got, want)
		}
	}
}

func TestIsContextOverflowOnlyMatchesThatReason(t *testing.T) {
	if !IsContextOverflow(fmt.Errorf("wrapped: %w", &APIError{Reason: ReasonContextOverflow})) {
		t.Error("want true for a wrapped context-overflow error")
	}
	if IsContextOverflow(&APIError{Reason: ReasonRateLimit}) {
		t.Error("want false for a rate-limit error")
	}
	if IsContextOverflow(errors.New("plain")) {
		t.Error("want false for an unclassified error")
	}
}
