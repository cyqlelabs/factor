package swarm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	fa2a "github.com/cyqlelabs/factor/internal/a2a"
	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/cost"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Turns is what the swarm needs from the agent loop.
type Turns interface {
	ProcessRemote(ctx context.Context, rt agent.RemoteTurn) (string, error)
	AudienceAt(channel, chatID string) string
}

// neverRemote are the tools no turn another agent started may use, whatever
// its grant: a remote agent cannot reconfigure, upgrade or re-pair this
// machine, start work that outlives the task, or tell this machine's room
// who is in it.
var neverRemote = []string{
	"config_set", "upgrade", "pkg_install", AdminToolName,
	"job_start", "cron", "room", "voice_speakers",
}

// guestTools are what a guest's tasks may use unless granted otherwise:
// asking, looking things up, and its own memory. No shell, no files, no
// screen — a guest is somebody else's agent.
var guestTools = []string{tools.AskToolName, "web_search", "web_fetch", "recall", "remember"}

// allowed is the tool rule for one member's task. It is applied when a tool
// runs, not only when the list is offered.
func allowed(g Grant, role string, hasMeta bool) func(string) bool {
	guest := role == RoleGuest
	return func(name string) bool {
		if slices.Contains(neverRemote, name) {
			return false
		}
		if name == SendToolName {
			// Passing the work on needs the grant and a task that says where
			// it has been: without the chain there is no way to refuse a loop.
			// A guest never passes work on.
			return g.Forward && hasMeta && !guest
		}
		if guest && len(g.Tools) == 0 {
			return slices.Contains(guestTools, name)
		}
		if name == tools.AskToolName || len(g.Tools) == 0 {
			return true
		}
		return slices.Contains(g.Tools, name)
	}
}

// remoteAsker sends a turn's questions back to the agent that asked.
type remoteAsker struct {
	ask func(ctx context.Context, q string, options []string) (string, error)
}

func (a remoteAsker) Ask(ctx context.Context, q tools.Question) (tools.Answer, error) {
	answer, err := a.ask(ctx, q.Prompt, q.Options)
	if err != nil {
		return tools.Answer{}, err
	}
	return tools.Answer{Text: tools.MatchOption(answer, q.Options)}, nil
}

type chainKey struct{}

// chainFrom is the extension a remote turn arrived with, for a swarm_send
// the turn makes to pass the work on.
func chainFrom(ctx context.Context) (fa2a.Meta, bool) {
	m, ok := ctx.Value(chainKey{}).(fa2a.Meta)
	return m, ok
}

// Run implements a2a.Runner: one task, as a turn under the member's grant.
func (s *Service) Run(ctx context.Context, t fa2a.Turn) (string, error) {
	if s.turns == nil {
		return "", errors.New("this agent is not running yet; its gateway takes tasks")
	}
	st := s.book.Read()
	if !st.Roster.Has(t.Peer) || t.Peer == s.id.ID {
		return "", fmt.Errorf("agent %s is not a member of this swarm", t.Peer)
	}
	if !st.Refreshed.IsZero() && s.now().Sub(st.Refreshed) > StaleAfter {
		return "", fmt.Errorf("this agent has not been able to confirm the swarm's membership for over %s, "+
			"so it takes no tasks until it reaches the rendezvous again", StaleAfter.Round(time.Hour))
	}
	peer := st.Peer(t.Peer)
	rm, _ := st.Roster.Member(t.Peer)
	if t.HasMeta {
		ctx = context.WithValue(ctx, chainKey{}, t.Meta)
	}
	reply, err := s.turns.ProcessRemote(ctx, agent.RemoteTurn{
		Peer:    t.Peer,
		Name:    st.Label(t.Peer),
		Context: t.Context,
		Task:    t.Task,
		Content: t.Text,
		Private: t.HasMeta && t.Meta.Private(),
		Ask:     remoteAsker{ask: t.Ask},
		Allowed: allowed(peer.Grant, rm.Role, t.HasMeta),
		Guest:   rm.Role == RoleGuest,
	})
	if err != nil {
		return "", failure(err)
	}
	if strings.TrimSpace(reply) == "" {
		return "", errors.New("the turn ended without an answer")
	}
	return reply, nil
}

// failure turns an error into the reason the other agent reads.
func failure(err error) error {
	var budget *cost.BudgetError
	if errors.As(err, &budget) {
		return errors.New(budget.Message())
	}
	var api *provider.APIError
	if errors.As(err, &api) && api.Reason == provider.ReasonBilling {
		return errors.New("this agent's model provider refused payment (402); its owner has to add credits")
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("the task was cancelled")
	}
	return fmt.Errorf("the turn failed: %v", err)
}
