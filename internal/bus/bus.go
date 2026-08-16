// Package bus decouples channels from the agent loop with bounded queues.
package bus

import (
	"log/slog"
	"time"
)

const defaultBuffer = 64

type InboundMessage struct {
	Channel  string
	SenderID string
	ChatID   string
	Content  string
	Time     time.Time
}

type OutboundMessage struct {
	Channel string
	ChatID  string
	Content string
	// Interim marks a note sent while a turn is still running — what the
	// agent is about to do, not its answer. Connectors whose delivery is
	// expensive or interruptive (a phone call) drop these.
	Interim bool
}

// SessionKey identifies the conversation a message belongs to.
func (m InboundMessage) SessionKey() string { return m.Channel + ":" + m.ChatID }

// External reports whether a channel is a conversation that outlives this
// process, and so is somewhere Factor can reach the user on its own
// initiative. The CLI is one process's stdin, system is Factor talking to
// itself, and cron is a schedule rather than an inbox: none of them is an
// address a heartbeat, a job result or a restart notice can be sent to.
func External(channel string) bool {
	switch channel {
	case "", "cli", "system", "cron":
		return false
	}
	return true
}

type MessageBus struct {
	inbound  chan InboundMessage
	outbound chan OutboundMessage
}

func New() *MessageBus {
	return &MessageBus{
		inbound:  make(chan InboundMessage, defaultBuffer),
		outbound: make(chan OutboundMessage, defaultBuffer),
	}
}

func (b *MessageBus) Inbound() <-chan InboundMessage   { return b.inbound }
func (b *MessageBus) Outbound() <-chan OutboundMessage { return b.outbound }

// PendingOutbound counts replies waiting for a connector to deliver them.
// The gateway waits for zero before restarting: a queued message dies with
// the process that holds it.
func (b *MessageBus) PendingOutbound() int { return len(b.outbound) }

// PublishInbound enqueues without blocking; a full queue drops the message
// loudly rather than wedging a connector.
func (b *MessageBus) PublishInbound(msg InboundMessage) bool {
	select {
	case b.inbound <- msg:
		return true
	default:
		slog.Error("inbound queue full, dropping message", "channel", msg.Channel, "chat", msg.ChatID)
		return false
	}
}

func (b *MessageBus) PublishOutbound(msg OutboundMessage) bool {
	select {
	case b.outbound <- msg:
		return true
	default:
		slog.Error("outbound queue full, dropping message", "channel", msg.Channel, "chat", msg.ChatID)
		return false
	}
}
