package memory

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

// audiencePolicy is testPolicy with the shared space configured — the whole
// feature switched on.
func audiencePolicy() SpacePolicy {
	p := testPolicy()
	p.Shared = "shared"
	return p
}

func sharedCtx() context.Context {
	return tools.WithToolContext(context.Background(),
		tools.ToolContext{Channel: "voice", ChatID: "local:room", Audience: tools.AudienceShared})
}

func privateCtx() context.Context {
	return tools.WithToolContext(context.Background(),
		tools.ToolContext{Channel: "voice", ChatID: "local"})
}

// The asymmetry is the feature: a shared turn reads only what was said in
// company, a private turn reads that plus everything else. Reversing either
// half breaks it — one leaks, the other loses the user's own conversation.
func TestScopeIsOneDirectional(t *testing.T) {
	p := audiencePolicy()

	shared, ok := p.Scope("voice", tools.AudienceShared)
	if !ok {
		t.Fatal("a shared turn was refused with a shared space configured")
	}
	if shared.Space != "shared" || !reflect.DeepEqual(shared.ReadSpaces, []string{"shared"}) {
		t.Errorf("shared scope = %+v, want write and read shared only", shared)
	}

	private, ok := p.Scope("voice", "")
	if !ok {
		t.Fatal("a private turn was refused")
	}
	if private.Space != "main" {
		t.Errorf("private write space = %q, want main", private.Space)
	}
	if !reflect.DeepEqual(private.ReadSpaces, []string{"main", "shared", "system"}) {
		t.Errorf("private read spaces = %v, want main, shared and system", private.ReadSpaces)
	}
}

// A machine turn is not a conversation and has no audience to speak of; it
// keeps the system split it always had.
func TestMachineTurnsIgnoreTheAudience(t *testing.T) {
	p := audiencePolicy()
	for _, channel := range []string{"cron", "job", "system"} {
		got, ok := p.Scope(channel, tools.AudienceShared)
		if !ok || got.Space != "system" {
			t.Errorf("Scope(%q, shared) = %+v, %v, want the system space", channel, got, ok)
		}
	}
}

// With no shared space there is nowhere to isolate to, and the one space left
// holds everything. Serving a guest from it is the exact leak this exists to
// stop, so recall is refused instead of quietly widened.
func TestSharedTurnWithNothingToIsolateIntoRefusesRecall(t *testing.T) {
	noShared := testPolicy() // Shared unset
	if _, ok := noShared.Scope("voice", tools.AudienceShared); ok {
		t.Error("a shared turn was served from the private space")
	}
	if _, ok := noShared.Scope("voice", ""); !ok {
		t.Error("a private turn was refused for no reason")
	}

	single := SpacePolicy{Strategy: "single", Main: "main", System: "system", Shared: "shared"}
	if _, ok := single.Scope("voice", tools.AudienceShared); ok {
		t.Error("single-space strategy served a shared turn")
	}
	if _, ok := (SpacePolicy{}).Scope("voice", tools.AudienceShared); ok {
		t.Error("a zero policy served a shared turn")
	}
}

// An engine that cannot route spaces is the same situation by another road.
func TestEngineWithoutRoutingRefusesASharedRecall(t *testing.T) {
	eng := &scopeEngine{} // no space support
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, audiencePolicy())

	if got := a.MemoryPrompt(sharedCtx(), nil, "what do you know about me"); got != "" {
		t.Errorf("recall served against an engine that cannot isolate: %q", got)
	}
	if !reflect.DeepEqual(eng.recallScope, Scope{}) {
		t.Error("the engine was asked to recall at all")
	}
	// The private turn on the same engine is untouched.
	if got := a.MemoryPrompt(privateCtx(), nil, "what do you know about me"); got == "" {
		t.Error("a private turn lost its recall too")
	}
}

func TestSharedRecallReadsOnlyTheSharedSpace(t *testing.T) {
	eng := newScopeEngine()
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, audiencePolicy())
	a.MemoryPrompt(sharedCtx(), nil, "what did we decide")
	if !reflect.DeepEqual(eng.recallScope.ReadSpaces, []string{"shared"}) {
		t.Errorf("shared recall read %v, want the shared space alone", eng.recallScope.ReadSpaces)
	}
}

// What was said in company is not a secret from anybody who was there, so it
// is stored — in the space both of them can read back.
func TestSharedTurnStoresWhereTheRoomCanReadIt(t *testing.T) {
	eng := newScopeEngine()
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, audiencePolicy())
	a.StoreExchange("voice", tools.AudienceShared, "Roxana", "we're going on Friday", "noted")
	if len(eng.remembered) == 0 {
		t.Fatal("nothing was stored")
	}
	for i, req := range eng.remembered {
		if req.Space != "shared" {
			t.Errorf("remembered[%d].Space = %q, want shared", i, req.Space)
		}
	}
}

// Even where recall had to be refused, the conversation is still written down:
// dropping it would lose the exchange for both people who were there.
func TestSharedTurnStillStoresWhenItCannotIsolate(t *testing.T) {
	eng := newScopeEngine()
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, testPolicy()) // no shared space
	a.StoreExchange("voice", tools.AudienceShared, "Roxana", "we're going on Friday", "noted")
	if len(eng.remembered) == 0 {
		t.Fatal("the exchange was dropped rather than stored unpartitioned")
	}
}

func TestPrivateTurnStoresToMain(t *testing.T) {
	eng := newScopeEngine()
	a := NewAmbient(eng, 5, 0.1, 5, 500, 500, nil, audiencePolicy())
	a.StoreExchange("voice", "", "", "a secret", "noted")
	for i, req := range eng.remembered {
		if req.Space != "main" {
			t.Errorf("remembered[%d].Space = %q, want main", i, req.Space)
		}
	}
}

// A shared space that is really one of the others is not a partition, and the
// config would claim a privacy boundary that does not exist.
func TestSharedSpaceMustBeItsOwn(t *testing.T) {
	for _, shared := range []string{"main", "system"} {
		if _, err := NewSpacePolicy("origin", "main", "system", shared); err == nil {
			t.Errorf("shared_space %q was accepted", shared)
		}
	}
	if _, err := NewSpacePolicy("origin", "main", "system", "shared"); err != nil {
		t.Errorf("a distinct shared space was rejected: %v", err)
	}
	if _, err := NewSpacePolicy("origin", "main", "system", ""); err != nil {
		t.Errorf("an unset shared space was rejected: %v", err)
	}
}

// Scoping a turn by who can hear it is worth nothing if naming a space walks
// around it, and the space parameter reads as an ordinary advanced option.
func TestToolsRefuseToReachAcrossTheAudience(t *testing.T) {
	eng := newScopeEngine()
	set := NewTools(eng, audiencePolicy())
	ctx := sharedCtx()

	for _, name := range []string{"remember", "recall", "forget"} {
		tool := toolByName(t, set, name)
		res := tool.Execute(ctx, map[string]any{
			"content": "x", "query": "x", "space": "main",
		})
		if res == nil || !res.IsError {
			t.Errorf("%s reached the private space with a guest present: %+v", name, res)
		}
	}
	// The shared space itself is still nameable.
	res := toolByName(t, set, "recall").Execute(ctx, map[string]any{"query": "x", "space": "shared"})
	if res != nil && res.IsError {
		t.Errorf("recall refused its own space: %+v", res)
	}
}

// A deliberate search is spoken out loud like any other answer.
func TestRecallToolIsOffWhenTheRoomCannotBeScoped(t *testing.T) {
	set := NewTools(newScopeEngine(), testPolicy()) // no shared space
	res := toolByName(t, set, "recall").Execute(sharedCtx(), map[string]any{"query": "anything"})
	if res == nil || !res.IsError || !strings.Contains(res.ForLLM, "somebody else is present") {
		t.Errorf("recall = %+v, want a refusal naming the room", res)
	}
}

// Writing is not a leak, so it keeps working where reading cannot.
func TestRememberStillWorksWhenTheRoomCannotBeScoped(t *testing.T) {
	eng := newScopeEngine()
	set := NewTools(eng, testPolicy())
	res := toolByName(t, set, "remember").Execute(sharedCtx(), map[string]any{"content": "we met today"})
	if res != nil && res.IsError {
		t.Errorf("remember = %+v, want it to store", res)
	}
	if len(eng.remembered) == 0 {
		t.Error("nothing was stored")
	}
}
