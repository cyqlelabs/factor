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
