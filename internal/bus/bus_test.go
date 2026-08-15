package bus

import (
	"testing"
	"time"
)

func TestSessionKey(t *testing.T) {
	msg := InboundMessage{Channel: "telegram", ChatID: "42"}
	if got := msg.SessionKey(); got != "telegram:42" {
		t.Errorf("SessionKey() = %q", got)
	}
	empty := InboundMessage{}
	if got := empty.SessionKey(); got != ":" {
		t.Errorf("empty SessionKey() = %q", got)
	}
}

func TestPublishAndReceive(t *testing.T) {
	b := New()
	in := InboundMessage{Channel: "cli", ChatID: "main", Content: "hello", Time: time.Now()}
	if !b.PublishInbound(in) {
		t.Fatal("PublishInbound returned false on an empty queue")
	}
	select {
	case got := <-b.Inbound():
		if got.Content != "hello" || got.Channel != "cli" {
			t.Errorf("inbound = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("inbound message never arrived")
	}

	out := OutboundMessage{Channel: "cli", ChatID: "main", Content: "reply"}
	if !b.PublishOutbound(out) {
		t.Fatal("PublishOutbound returned false on an empty queue")
	}
	select {
	case got := <-b.Outbound():
		if got.Content != "reply" {
			t.Errorf("outbound = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("outbound message never arrived")
	}
}

func TestPublishDropsWhenFullInsteadOfBlocking(t *testing.T) {
	// A wedged consumer must never block a connector: the bus drops loudly.
	b := New()
	for i := range defaultBuffer {
		if !b.PublishInbound(InboundMessage{Channel: "t", ChatID: "1"}) {
			t.Fatalf("inbound publish %d failed before the buffer was full", i)
		}
	}
	done := make(chan bool, 1)
	go func() { done <- b.PublishInbound(InboundMessage{Channel: "t", ChatID: "1"}) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("publish onto a full queue reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("PublishInbound blocked on a full queue")
	}

	for i := range defaultBuffer {
		if !b.PublishOutbound(OutboundMessage{Channel: "t", ChatID: "1"}) {
			t.Fatalf("outbound publish %d failed before the buffer was full", i)
		}
	}
	go func() { done <- b.PublishOutbound(OutboundMessage{Channel: "t", ChatID: "1"}) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("outbound publish onto a full queue reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("PublishOutbound blocked on a full queue")
	}
}

func TestPendingOutbound(t *testing.T) {
	b := New()
	if b.PendingOutbound() != 0 {
		t.Fatalf("a fresh bus has %d replies waiting", b.PendingOutbound())
	}
	b.PublishOutbound(OutboundMessage{Channel: "telegram", ChatID: "1", Content: "restarting"})
	b.PublishOutbound(OutboundMessage{Channel: "telegram", ChatID: "1", Content: "back"})
	if b.PendingOutbound() != 2 {
		t.Errorf("PendingOutbound() = %d, want 2", b.PendingOutbound())
	}
	<-b.Outbound()
	<-b.Outbound()
	if b.PendingOutbound() != 0 {
		t.Errorf("PendingOutbound() = %d after both were delivered", b.PendingOutbound())
	}
}

func TestQueuesAreIndependent(t *testing.T) {
	b := New()
	for range defaultBuffer {
		b.PublishInbound(InboundMessage{Channel: "t", ChatID: "1"})
	}
	// a saturated inbound queue must not affect outbound delivery
	if !b.PublishOutbound(OutboundMessage{Channel: "t", ChatID: "1", Content: "still works"}) {
		t.Error("outbound publish blocked by a full inbound queue")
	}
}

func TestFIFOOrder(t *testing.T) {
	b := New()
	for i := range 5 {
		b.PublishInbound(InboundMessage{Channel: "cli", ChatID: "1", Content: string(rune('a' + i))})
	}
	for i := range 5 {
		got := <-b.Inbound()
		if want := string(rune('a' + i)); got.Content != want {
			t.Errorf("message %d = %q, want %q", i, got.Content, want)
		}
	}
}
