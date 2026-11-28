package agent

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/cost"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

// withSharedSpace rebuilds the harness's memory with a shared space, so a
// shared turn has somewhere of its own to recall from.
func withSharedSpace(h *harness) {
	ambient := memory.NewAmbient(h.engine, 5, 0.3, 5, 500, 500, nil,
		memory.SpacePolicy{Strategy: "origin", Main: "main", System: "system", Shared: "shared"})
	h.loop.ambient = ambient
	h.loop.builder = NewContextBuilder(h.loop.cfg, skills.NewLoader(filepath.Join(h.loop.cfg.Agent.Workspace, "skills")), ambient)
}

type answerAsker struct{ asked []string }

func (a *answerAsker) Ask(_ context.Context, q tools.Question) (tools.Answer, error) {
	a.asked = append(a.asked, q.Prompt)
	return tools.Answer{Text: "the red one"}, nil
}

func TestProcessRemoteIsNotAPersonsTurn(t *testing.T) {
	var offered []string
	h := newHarness(t, func(req *provider.Request) (*provider.Response, error) {
		for _, d := range req.Tools {
			offered = append(offered, d.Name)
		}
		return &provider.Response{ToolCalls: []provider.ToolCall{
			{ID: "1", Name: "probe", Args: map[string]any{"value": "x"}},
			{ID: "2", Name: tools.AskToolName, Args: map[string]any{"question": "which one?"}},
		}}, nil
	}, func(req *provider.Request) (*provider.Response, error) {
		return &provider.Response{Content: "done"}, nil
	})
	withSharedSpace(h)
	h.registry.Register(tools.NewAskTool(nil))
	asker := &answerAsker{}
	reply, err := h.loop.ProcessRemote(context.Background(), RemoteTurn{
		Peer: "peerid", Name: "desk", Context: "ctx1", Task: "t1", Content: "check it",
		Ask:     asker,
		Allowed: func(name string) bool { return name != "probe" },
	})
	if err != nil || reply != "done" {
		t.Fatalf("reply %q err %v", reply, err)
	}
	if slices.Contains(offered, "probe") {
		t.Error("a withheld tool was offered")
	}
	if len(h.tool.calls) != 0 {
		t.Error("a withheld tool ran")
	}
	if len(asker.asked) != 1 || asker.asked[0] != "which one?" {
		t.Errorf("questions went to %v, not the caller", asker.asked)
	}
	if _, _, ok := h.loop.LastChannel(); ok {
		t.Error("another agent's task became the last chat")
	}
	history, _ := h.store.History(RemoteSession("peerid", "ctx1"))
	if len(history) == 0 || !strings.Contains(history[0].Content, "[desk] check it") {
		t.Errorf("history %v", history)
	}
	// The caller did not vouch for a private room, so only shared memory was
	// read.
	h.engine.mu.Lock()
	scopes := h.engine.scopes
	h.engine.mu.Unlock()
	if len(scopes) == 0 {
		t.Fatal("nothing was recalled")
	}
	for _, sc := range scopes {
		if sc.Space != "shared" || slices.Contains(sc.ReadSpaces, "main") {
			t.Errorf("a shared remote task recalled %+v", sc)
		}
	}
	// And the turn context told the model who it is answering.
	last := h.chat.requests[0].Messages
	var brief bool
	for _, m := range last {
		if strings.Contains(m.Content, "another Factor agent in your swarm") {
			brief = true
		}
	}
	if !brief {
		t.Error("the turn was not briefed as another agent's")
	}
	if triggerOf(bus.InboundMessage{Channel: remoteChannel}) != "a2a" || bus.External(remoteChannel) {
		t.Error("an a2a turn reads as a person's")
	}
}

func TestPrivateRemoteTaskReadsPrivateMemory(t *testing.T) {
	h := newHarness(t)
	withSharedSpace(h)
	if _, err := h.loop.ProcessRemote(context.Background(), RemoteTurn{
		Peer: "p", Context: "c", Content: "hi", Private: true}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.scopes) == 0 || !slices.Contains(h.engine.scopes[0].ReadSpaces, "main") {
		t.Errorf("a private remote task recalled %+v", h.engine.scopes)
	}
}

func TestRemoteBudgetStopIsAnError(t *testing.T) {
	h := newHarness(t, func(*provider.Request) (*provider.Response, error) {
		return nil, &cost.BudgetError{Scope: "member", Spent: 1, Limit: 1}
	})
	_, err := h.loop.ProcessRemote(context.Background(), RemoteTurn{Peer: "p", Context: "c", Content: "hi"})
	var be *cost.BudgetError
	if !errors.As(err, &be) {
		t.Errorf("err %v, want the budget error", err)
	}
	if _, err := h.loop.ProcessRemote(context.Background(), RemoteTurn{Content: "hi"}); err == nil {
		t.Error("a remote turn without a peer ran")
	}
}

func TestRemoteBriefing(t *testing.T) {
	if strings.Contains(remoteBriefing(""), "shared memory") {
		t.Error("a private task was told it is shared")
	}
	if !strings.Contains(remoteBriefing(tools.AudienceShared), "shared memory") {
		t.Error("a shared task was not told")
	}
	if !strings.Contains(remoteBriefing(""), "not among yours, this machine cannot do it") {
		t.Error("a task is not told to say what this machine lacks")
	}
	tc := tools.ToolContext{Channel: remoteChannel, Audience: tools.AudienceShared}
	if !strings.Contains(channelBriefing(tc), "another Factor agent") {
		t.Error("channelBriefing ignores a2a")
	}
	if !(tools.ToolContext{}).ByPerson() || (tools.ToolContext{Trigger: "a2a"}).ByPerson() {
		t.Error("ByPerson")
	}
}

func TestGuestRemoteTaskReadsOnlyTheGuestSpace(t *testing.T) {
	h := newHarness(t)
	spaces, _ := memory.NewSpacePolicy("origin", "main", "system", "shared")
	spaces, _ = spaces.WithGuest("guest")
	ambient := memory.NewAmbient(h.engine, 5, 0.3, 5, 500, 500, nil, spaces)
	h.loop.ambient = ambient
	h.loop.builder = NewContextBuilder(h.loop.cfg, skills.NewLoader(filepath.Join(h.loop.cfg.Agent.Workspace, "skills")), ambient)
	if _, err := h.loop.ProcessRemote(context.Background(), RemoteTurn{
		Peer: "p", Context: "c", Content: "hi", Private: true, Guest: true}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	for _, sc := range h.engine.scopes {
		if sc.Space != "guest" || len(sc.ReadSpaces) != 1 {
			t.Errorf("a guest's task recalled %+v", sc)
		}
	}
	if !strings.Contains(remoteBriefing(tools.AudienceGuest), "guest") {
		t.Error("guest briefing")
	}
}

func TestBusMessagesForRemoteSessionsAreDropped(t *testing.T) {
	h := newHarness(t)
	h.loop.dispatch(context.Background(), bus.InboundMessage{Channel: remoteChannel, ChatID: "p:c", Content: "hi"})
	h.loop.WaitBackground(time.Second)
	if len(h.chat.requests) != 0 {
		t.Error("a bus message ran a turn on another agent's session")
	}
}
