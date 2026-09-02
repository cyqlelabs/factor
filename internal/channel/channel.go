// Package channel defines the connector seam. Connectors self-register via
// Register (one package + one init line each) and decode their own raw JSON
// config section, so adding WhatsApp/Twilio/Slack/... never touches core.
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Channel is one chat connector. Start must be non-blocking (spawn your own
// goroutines off ctx); Send delivers one already-chunked message.
type Channel interface {
	Name() string
	Start(ctx context.Context) error
	Stop() error
	Send(ctx context.Context, msg bus.OutboundMessage) error
	MaxMessageLength() int // 0 = unlimited
}

// Typer is the optional capability of showing the user that a turn is being
// worked on. Connectors whose protocol has no such signal simply omit it.
type Typer interface {
	SetTyping(chatID string, on bool)
}

// TurnFunc runs one synchronous turn and returns the reply (wired to
// Loop.ProcessDirectNotice). notice is called with each line the agent says
// on its way to that reply — "let me look that up" — as it says it, so a turn
// spent in tool calls is audible progress rather than a silence the user
// cannot tell from a hang. It runs on the turn's own goroutine, so a
// connector that takes time to deliver a note must not block in it.
// speaker names who is talking where the connector can tell (the microphone
// recognizing a household voice); blank means "whoever this chat is".
// audience says who can hear the reply — tools.AudienceShared where the
// connector knows somebody besides the user is present, blank everywhere
// else. It is separate from speaker because the two answer different
// questions: speaker decides attribution, audience decides discretion.
type TurnFunc func(ctx context.Context, content, sessionKey, speaker, audience string, notice func(string)) (string, error)

// TurnRunner is the optional capability of a connector that runs turns itself
// instead of publishing them onto the bus. A phone call is synchronous — the
// caller is waiting on the line, and hanging up must cancel the turn — so the
// bus's fire-and-forget shape does not fit it.
type TurnRunner interface {
	BindTurnRunner(run TurnFunc)
}

// Steerable is the optional capability of a TurnRunner whose replies also
// reach the user through Send. It says a turn this connector starts may be
// folded into one already live for the same session — a background job
// reporting back, a cron result — instead of queuing behind it: the words
// land while they are still what the conversation is about, and the running
// turn's reply comes back the usual way. Without it a connector's turn waits
// for the session, which is what a phone call needs, since a reply published
// to the bus would be dialled as a second call rather than spoken on the
// line the caller is holding.
type Steerable interface {
	AcceptsSteering()
}

// BindTurns wires a connector's turn runner to the entry point its replies
// can come back through: steer where Steerable says the bus can deliver
// them, wait where they have to be returned here. A connector that does not
// run its own turns is left alone.
func BindTurns(ch Channel, wait, steer TurnFunc) {
	runner, ok := ch.(TurnRunner)
	if !ok {
		return
	}
	if _, steers := ch.(Steerable); steers {
		runner.BindTurnRunner(steer)
		return
	}
	runner.BindTurnRunner(wait)
}

// Toolset is the optional capability of contributing tools that only make
// sense where the connector is configured, so a machine without it never sees
// a tool that could only fail.
type Toolset interface {
	Toolset() []tools.Tool
}

// Guarded is the optional capability of a connector that reads or writes
// files itself — sending a chat a local file, saving one it received — and so
// must obey the same path rules as every file tool. Bound before Start.
type Guarded interface {
	BindPathGuard(*tools.PathGuard)
}

// Addresser is the optional capability of a connector that sometimes has to
// hand a message to the user's usual written conversation — a spoken exchange
// asked to answer in writing. The host binds where that is: the gateway hands
// it the loop's last external chat, the CLI its own terminal session. Bound
// before Start.
type Addresser interface {
	BindLastExternal(func() (channel, chatID string, ok bool))
}

// Localized is the optional capability of a connector whose replies are heard
// in one fixed language: a synthesized voice speaks the language it was built
// for and reads anything else in the wrong accent. A written chat has no such
// fix — its language is whatever the user writes in — and omits it. The
// answer is the language code the connector is configured with ("es").
type Localized interface {
	Language() string
}

// Factory builds a channel from its raw config section.
type Factory func(raw json.RawMessage, b *bus.MessageBus) (Channel, error)

var factories = map[string]Factory{}

// Register installs a connector factory (call from the connector's init).
func Register(name string, f Factory) { factories[name] = f }

// Registered lists known connector names.
func Registered() []string {
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	return names
}

// Build instantiates every enabled configured channel. Unknown or disabled
// sections are skipped with a log line, never an error, so configs stay
// forward-compatible.
func Build(cfgs map[string]json.RawMessage, b *bus.MessageBus) []Channel {
	var out []Channel
	for name, raw := range cfgs {
		factory, ok := factories[name]
		if !ok {
			slog.Warn("no connector registered for channel; skipping", "channel", name, "known", Registered())
			continue
		}
		var gate struct {
			Enabled *bool `json:"enabled"`
		}
		_ = json.Unmarshal(raw, &gate)
		if gate.Enabled != nil && !*gate.Enabled {
			continue
		}
		ch, err := factory(raw, b)
		if err != nil {
			slog.Error("channel failed to build; skipping", "channel", name, "error", err)
			continue
		}
		out = append(out, ch)
	}
	return out
}

// Validate builds every enabled, known section against a throwaway bus and
// reports the first one its connector rejects. It is what the gateway checks
// before reloading itself over a config edit: Build skips a broken section
// with a log line, which under a live reload would silently drop a channel
// the user only mistyped. Nothing is started; the constructions are discarded.
func Validate(cfgs map[string]json.RawMessage) error {
	b := bus.New()
	for name, raw := range cfgs {
		factory, ok := factories[name]
		if !ok {
			continue // unknown sections stay forward-compatible, as in Build
		}
		var gate struct {
			Enabled *bool `json:"enabled"`
		}
		_ = json.Unmarshal(raw, &gate)
		if gate.Enabled != nil && !*gate.Enabled {
			continue
		}
		if _, err := factory(raw, b); err != nil {
			return fmt.Errorf("channels.%s: %w", name, err)
		}
	}
	return nil
}

// SplitMessage chunks content at a channel's length limit, preferring
// newline then space boundaries.
func SplitMessage(content string, limit int) []string {
	if limit <= 0 || len(content) <= limit {
		return []string{content}
	}
	var chunks []string
	for len(content) > limit {
		cut := limit
		if idx := lastIndexByteBefore(content, '\n', limit); idx > limit/2 {
			cut = idx
		} else if idx := lastIndexByteBefore(content, ' ', limit); idx > limit/2 {
			cut = idx
		}
		chunks = append(chunks, content[:cut])
		content = content[cut:]
		for len(content) > 0 && (content[0] == '\n' || content[0] == ' ') {
			content = content[1:]
		}
	}
	if len(content) > 0 {
		chunks = append(chunks, content)
	}
	return chunks
}

func lastIndexByteBefore(s string, b byte, before int) int {
	for i := before - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// ErrUnknownChannel reports an outbound message for a channel the manager
// does not own.
var ErrUnknownChannel = fmt.Errorf("unknown outbound channel")
