package channel

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
)

// Manager starts channels and pumps the outbound bus to them with chunking
// and bounded retry.
type Manager struct {
	b        *bus.MessageBus
	channels map[string]Channel
	wg       sync.WaitGroup
	sleep    func(ctx context.Context, d time.Duration)
}

func NewManager(b *bus.MessageBus, channels []Channel) *Manager {
	m := &Manager{b: b, channels: map[string]Channel{}, sleep: sleepCtx}
	for _, ch := range channels {
		m.channels[ch.Name()] = ch
	}
	return m
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (m *Manager) Names() []string {
	names := make([]string, 0, len(m.channels))
	for n := range m.channels {
		names = append(names, n)
	}
	return names
}

// Start launches every channel and the outbound pump.
func (m *Manager) Start(ctx context.Context) {
	for name, ch := range m.channels {
		if err := ch.Start(ctx); err != nil {
			slog.Error("channel failed to start", "channel", name, "error", err)
		} else {
			slog.Info("channel started", "channel", name)
		}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-m.b.Outbound():
				m.deliver(ctx, msg)
			}
		}
	}()
}

// SetTyping routes a session's busy state to the channel it belongs to, for
// connectors that can show one. Session keys of channels this manager does
// not own (cli, cron, heartbeat) are ignored.
func (m *Manager) SetTyping(sessionKey string, on bool) {
	name, chatID, ok := strings.Cut(sessionKey, ":")
	if !ok {
		return
	}
	if typer, can := m.channels[name].(Typer); can {
		typer.SetTyping(chatID, on)
	}
}

// Interim delivers a note from a turn that is still running to the chat it
// belongs to, so a long turn reads as work in progress rather than silence.
// It goes through the outbound bus like any reply, keeping it in order with
// the answer that follows. Sessions of channels this manager does not own
// (cli, cron, jobs) are ignored.
func (m *Manager) Interim(sessionKey, content string) {
	name, chatID, ok := strings.Cut(sessionKey, ":")
	if !ok {
		return
	}
	if _, owned := m.channels[name]; !owned {
		return
	}
	m.b.PublishOutbound(bus.OutboundMessage{Channel: name, ChatID: chatID, Content: content, Interim: true})
}

func (m *Manager) deliver(ctx context.Context, msg bus.OutboundMessage) {
	ch, ok := m.channels[msg.Channel]
	if !ok {
		slog.Warn("dropping outbound for unknown channel", "channel", msg.Channel)
		return
	}
	for _, chunk := range SplitMessage(msg.Content, ch.MaxMessageLength()) {
		part := bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Content: chunk, Interim: msg.Interim}
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = ch.Send(ctx, part); err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			m.sleep(ctx, time.Duration(attempt+1)*2*time.Second)
		}
		if err != nil {
			slog.Error("send failed after retries", "channel", msg.Channel, "error", err)
			return // don't send later chunks out of order after a hard failure
		}
	}
}

// Stop stops all channels and waits for the pump to drain.
func (m *Manager) Stop() {
	for name, ch := range m.channels {
		if err := ch.Stop(); err != nil {
			slog.Warn("channel stop error", "channel", name, "error", err)
		}
	}
	m.wg.Wait()
}
