package channel

import (
	"context"
	"log/slog"
	"sort"
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

	// mu guards what only becomes true once the manager runs: which
	// connectors actually came up, and how many messages are in the middle
	// of being handed to one.
	mu       sync.Mutex
	started  map[string]bool
	failed   map[string]string
	inFlight int
}

func NewManager(b *bus.MessageBus, channels []Channel) *Manager {
	m := &Manager{b: b, channels: map[string]Channel{}, sleep: sleepCtx,
		started: map[string]bool{}, failed: map[string]string{}}
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

// Serves reports whether a connector is running for this channel — whether
// a message addressed to it can actually reach the user.
//
// Configured, started and running are three different states, and only the
// last one is an address. A connector whose Start returned an error is kept
// in the map so it can be stopped and reported on, but nothing it was asked
// to deliver would arrive: a heartbeat or a restart notice sent there is
// logged as delivered and then dropped by the pump.
func (m *Manager) Serves(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started[name]
}

// Failed names the connectors that were configured but did not come up, with
// what they said. It is what a status line and the health endpoint report:
// "channels: telegram" beside a silent voice channel is a lie a user has no
// other way to catch.
func (m *Manager) Failed() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.failed))
	for k, v := range m.failed {
		out[k] = v
	}
	return out
}

// Running lists the connectors that are actually serving, sorted so a status
// line reads the same twice.
func (m *Manager) Running() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.started))
	for name, ok := range m.started {
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// InFlight counts the outbound messages that have left the queue but have not
// finished being handed to their connector — a send on the wire, a retry
// waiting out its backoff, a paragraph still on the speakers. The queue
// length alone says nothing about those: a message is invisible to it from
// the moment the pump picks it up, which is exactly the window a restart must
// not fire in.
func (m *Manager) InFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inFlight
}

// Audience asks a connector who can hear a reply on one of its chats right
// now. Blank for every connector that cannot tell, which is all of them but
// the microphone.
func (m *Manager) Audience(name, chatID string) string {
	if a, ok := m.channels[name].(Audiencer); ok {
		return a.Audience(chatID)
	}
	return ""
}

// Conversational reports whether a running connector is a chat whose
// conversation rides the bus both ways: a message published outbound lands in
// the chat, and what the user writes back comes in as inbound. A connector
// that runs its own turns (voice, the phone) is not one, whatever else it
// publishes — text pushed onto the bus for it is spoken or dialled rather
// than threaded into the conversation, so a question sent that way could
// never have its answer come back the same way.
func (m *Manager) Conversational(name string) bool {
	ch, ok := m.channels[name]
	if !ok {
		return false
	}
	_, runsOwnTurns := ch.(TurnRunner)
	return !runsOwnTurns
}

// Language reports the language a connector's replies are heard in, where
// it fixes one (see Localized). Blank for a written chat and for a channel
// nothing serves.
func (m *Manager) Language(name string) string {
	if localized, ok := m.channels[name].(Localized); ok {
		return localized.Language()
	}
	return ""
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
		err := ch.Start(ctx)
		m.mu.Lock()
		m.started[name] = err == nil
		if err != nil {
			m.failed[name] = err.Error()
		}
		m.mu.Unlock()
		if err != nil {
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
				m.track(1)
				m.deliver(ctx, msg)
				m.track(-1)
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

// track moves the in-flight count as the pump picks a message up and puts it
// down again.
func (m *Manager) track(delta int) {
	m.mu.Lock()
	m.inFlight += delta
	m.mu.Unlock()
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
