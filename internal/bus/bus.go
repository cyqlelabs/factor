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
}

// SessionKey identifies the conversation a message belongs to.
func (m InboundMessage) SessionKey() string { return m.Channel + ":" + m.ChatID }

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
