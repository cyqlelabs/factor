// Package tools defines the tool seam and the built-in arsenal. New tools
// implement Tool and register in one line; the user curates the set via
// config (tools.disabled). Packages with heavier dependencies (memory, mcp,
// cron) define their tools locally and register them at the composition root.
package tools

import (
	"context"
	"fmt"

	"github.com/cyqlelabs/factor/internal/provider"
)

// Tool is the single seam every capability implements.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON Schema
	Execute(ctx context.Context, args map[string]any) *Result
}

// Parallel is the optional capability a tool declares when one of its calls
// may run at the same time as the other calls in the same batch.
//
// It is opt-in because sequence is load-bearing for most of this arsenal: a
// mouse move and the click after it mean something in order and nothing at
// once, a browser navigation changes what the next read returns, and two
// writes to one file race. The tools that gain from concurrency are the ones
// that only read and spend their time waiting — a file, a page, the memory
// sidecar — and a batch of those currently costs the sum of its round trips
// rather than the longest of them.
//
// The arguments are passed because safety is often a property of the call
// rather than of the tool: the same tool may read on one call and write on
// the next.
type Parallel interface {
	ParallelSafe(args map[string]any) bool
}

// ReadOnly is embedded by tools whose every call is a read, which makes them
// safe to run beside anything else in their batch.
type ReadOnly struct{}

// ParallelSafe implements Parallel.
func (ReadOnly) ParallelSafe(map[string]any) bool { return true }

// Result separates what the LLM sees from what the user sees.
type Result struct {
	ForLLM  string // fed back into the model (secret-filtered by the registry)
	ForUser string // optional direct user-visible note
	IsError bool
	// Images are shown to the model alongside ForLLM (an annotated
	// screenshot from screen_view, say). The agent loop attaches them to
	// the in-flight turn only: pruned once newer frames arrive, never
	// persisted to session history.
	Images []provider.ImagePart
}

func Text(s string) *Result             { return &Result{ForLLM: s} }
func Textf(f string, a ...any) *Result  { return &Result{ForLLM: fmt.Sprintf(f, a...)} }
func Errorf(f string, a ...any) *Result { return &Result{ForLLM: fmt.Sprintf(f, a...), IsError: true} }

// ToolContext carries request-scoped routing data to tools that need it
// (e.g. cron capturing the channel a job was created from).
type ToolContext struct {
	Channel    string
	ChatID     string
	SessionKey string
	// Audience is who can hear this turn's reply. Blank means the ordinary
	// private conversation; AudienceShared means somebody else is present.
	Audience string
	// Outlet is the channel the reply comes out on, where that is not the
	// one the turn runs under: a heartbeat runs as "system" and a scheduled
	// task as "cron" — which is what the tools and the memory scope key on —
	// and both are delivered to a chat the user used. Blank means Channel.
	Outlet string
	// Language is the language the reply is heard in, where the outlet fixes
	// one: a synthesized voice speaks the language it was built for and
	// reads anything else as noise. Blank where the reply is read, and the
	// user's own language is the one to match.
	Language string
}

// AudienceShared marks a turn that more than the user can hear — a second
// voice in the room. What it buys is memory scope: a shared turn must not
// recall what was said in private, and what it stores must stay reachable to
// everyone who was there. Blank is private and is the default everywhere, so
// a channel that cannot tell who is listening behaves exactly as before.
const AudienceShared = "shared"

type ctxKey struct{}

func WithToolContext(ctx context.Context, tc ToolContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, tc)
}

func ToolContextFrom(ctx context.Context) ToolContext {
	tc, _ := ctx.Value(ctxKey{}).(ToolContext)
	return tc
}

// Arg helpers: JSON numbers arrive as float64; these normalize access.

func StringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func IntArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func FloatArg(args map[string]any, key string, def float64) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func BoolArg(args map[string]any, key string, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}
