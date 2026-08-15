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

// TurnFunc runs one synchronous turn and returns the reply
// (wired to Loop.ProcessDirect).
type TurnFunc func(ctx context.Context, content, sessionKey string) (string, error)

// TurnRunner is the optional capability of a connector that runs turns itself
// instead of publishing them onto the bus. A phone call is synchronous — the
// caller is waiting on the line, and hanging up must cancel the turn — so the
// bus's fire-and-forget shape does not fit it.
type TurnRunner interface {
	BindTurnRunner(run TurnFunc)
}

// Toolset is the optional capability of contributing tools that only make
// sense where the connector is configured, so a machine without it never sees
// a tool that could only fail.
type Toolset interface {
	Toolset() []tools.Tool
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
