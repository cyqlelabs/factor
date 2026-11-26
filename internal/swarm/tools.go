package swarm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	fa2a "github.com/cyqlelabs/factor/internal/a2a"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool names. The admin tool is never offered to a turn another agent
// started, and swarm_send only when the caller's grant lets work move on.
const (
	AdminToolName = "swarm"
	SendToolName  = "swarm_send"
)

// errNotPerson is the refusal for an admin action a person did not ask for.
var errNotPerson = errors.New("only a turn the user started can change the swarm; a scheduled task, a heartbeat, " +
	"a job report or another agent cannot")

type sendTool struct{ s *Service }

func (t *sendTool) Name() string { return SendToolName }

func (t *sendTool) Description() string {
	return "Hand work to another agent in this swarm — another machine running Factor — or to an agent outside " +
		"it that the user named in swarm.agents, and get its answer. Use it when the user asks for something " +
		"that has to happen on that machine: its disk, its files, its screen, its network. Name the agent as " +
		"the user does. A quick answer comes back here; slow work " +
		"carries on in the background and its outcome arrives later as a system message. If the agent asks a " +
		"question, put it to the user, then call this again with the task id and their answer."
}

func (t *sendTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent":   map[string]any{"type": "string", "description": "The agent's name, e.g. \"home box\""},
			"message": map[string]any{"type": "string", "description": "What to ask or tell it, complete enough to act on without this conversation"},
			"task":    map[string]any{"type": "string", "description": "Only when answering a question the agent asked: the task id it came with"},
		},
		"required": []any{"agent", "message"},
	}
}

func (t *sendTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	s := t.s
	member, err := s.resolve(tools.StringArg(args, "agent"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	message := strings.TrimSpace(tools.StringArg(args, "message"))
	if message == "" {
		return tools.Errorf("message must not be empty")
	}
	tc := tools.ToolContextFrom(ctx)
	req := sendRequest{member: member, message: message, task: strings.TrimSpace(tools.StringArg(args, "task")),
		origin: Origin{Channel: tc.Channel, ChatID: tc.ChatID, Session: tc.SessionKey, Audience: tc.Audience},
		wait:   writtenWait}
	if spoken(tc) {
		req.wait = spokenWait
	}
	if req.meta, req.hold, err = s.chain(ctx, tc); err != nil {
		return tools.Errorf("%v", err)
	}
	if err := s.mayTell(member, tc); err != nil {
		return tools.Errorf("%v", err)
	}
	if req.meta.Loops(member.ID) {
		return tools.Errorf("%s has already handled this work on its way here; sending it back would be a loop", member.Name)
	}
	if req.meta.Hops > fa2a.MaxHops {
		return tools.Errorf("this work has already passed through %d agents, the most allowed", fa2a.MaxHops)
	}
	text, err := s.send(ctx, req)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Text(text)
}

// chain is the extension a message carries. Work another agent asked for
// keeps the chain it arrived with, one hop further, so it can never come
// back around — and its turn holds on until the answer, which its own caller
// is waiting on. Work the user asked for starts a chain here.
func (s *Service) chain(ctx context.Context, tc tools.ToolContext) (meta fa2a.Meta, hold bool, err error) {
	if tc.Channel == "a2a" {
		incoming, ok := chainFrom(ctx)
		if !ok {
			return fa2a.Meta{}, false, errors.New("this task arrived without the swarm extension, so it cannot be passed on")
		}
		return incoming.Next(s.id.ID), true, nil
	}
	audience := fa2a.AudienceShared
	if tc.Audience == "" {
		audience = fa2a.AudiencePrivate
	}
	return fa2a.Meta{Audience: audience}.Next(s.id.ID), false, nil
}

// mayTell applies an outside agent's grant: only a request the user makes
// reaches it, and from a private conversation only if granted.
func (s *Service) mayTell(member Member, tc tools.ToolContext) error {
	if !isExternal(member.ID) {
		return nil
	}
	a, _ := s.external(member.ID)
	switch {
	case !tc.ByPerson():
		return fmt.Errorf("%s is outside the swarm; only a request the user makes reaches it", member.Name)
	case tc.Audience == "" && !a.Private:
		return fmt.Errorf("%s is outside the swarm and not granted private conversations; the user can grant it "+
			"with swarm.agents.%s.private", member.Name, member.Name)
	}
	return nil
}

func spoken(tc tools.ToolContext) bool {
	for _, ch := range []string{tc.Channel, tc.Outlet} {
		if ch == "voice" || ch == "phone" {
			return true
		}
	}
	return false
}

type adminTool struct{ s *Service }

func (t *adminTool) Name() string { return AdminToolName }

func (t *adminTool) Description() string {
	return "Manage this agent's swarm: the other machines running Factor that it hands work to. " +
		"action=list shows the members; status shows what was handed to them and how it is going (use it for " +
		"\"how's it going?\"); invite adds a machine — the code goes to the user directly, never to you; join " +
		"joins a swarm with a code the user gives directly; remove takes a member out for good; rename changes " +
		"what you call a member; grant sets which tools a member's tasks may use here and whether they may pass " +
		"work on. Inviting, joining, removing and granting need the user's own request in this conversation."
}

func (t *adminTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string",
				"enum": []any{"list", "status", "invite", "join", "remove", "rename", "grant"}},
			"agent": map[string]any{"type": "string", "description": "The member, for remove, rename and grant"},
			"name":  map[string]any{"type": "string", "description": "For rename: the new name"},
			"role": map[string]any{"type": "string", "enum": []any{"member", "guest"},
				"description": "For invite and join: member (the default) for the user's own machines, guest for somebody else's agent"},
			"tools": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "For grant: the only tools the member's tasks may use here. Leave out for all of them"},
			"forward":  map[string]any{"type": "boolean", "description": "For grant: whether the member's tasks may pass work on to a third agent"},
			"question": map[string]any{"type": "string", "description": "For join: the question that asks the user for the code, in their language. Their answer goes to the pairing, not to you"},
		},
		"required": []any{"action"},
	}
}

func (t *adminTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	s := t.s
	tc := tools.ToolContextFrom(ctx)
	action := tools.StringArg(args, "action")
	switch action {
	case "list":
		return tools.Text(s.listText())
	case "status":
		return tools.Text(s.statusText())
	case "rename":
		return t.rename(args)
	}
	if !tc.ByPerson() {
		return tools.Errorf("%v", errNotPerson)
	}
	switch action {
	case "grant":
		return t.grant(args)
	case "invite":
		return s.invite(ctx, tc, tools.StringArg(args, "role"))
	case "join":
		return s.join(ctx, tc, tools.StringArg(args, "question"), tools.StringArg(args, "role"))
	case "remove":
		return s.removeMember(ctx, tools.StringArg(args, "agent"))
	}
	return tools.Errorf("unknown action %q", action)
}

func (t *adminTool) rename(args map[string]any) *tools.Result {
	s := t.s
	st := s.book.Read()
	m, err := st.Resolve(s.id.ID, tools.StringArg(args, "agent"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	name := strings.TrimSpace(tools.StringArg(args, "name"))
	if name == "" {
		return tools.Errorf("say what to call it")
	}
	if err := s.book.Update(func(st *State) error {
		st.peer(m.ID).Name = name
		return nil
	}); err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Textf("%s is now called %s here.", m.Name, name)
}

func (t *adminTool) grant(args map[string]any) *tools.Result {
	s := t.s
	st := s.book.Read()
	m, err := st.Resolve(s.id.ID, tools.StringArg(args, "agent"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	var list []string
	if raw, ok := args["tools"].([]any); ok {
		for _, v := range raw {
			if name, ok := v.(string); ok && strings.TrimSpace(name) != "" {
				list = append(list, strings.TrimSpace(name))
			}
		}
	}
	forward := tools.BoolArg(args, "forward", m.Grant.Forward)
	if err := s.book.Update(func(st *State) error {
		st.peer(m.ID).Grant = Grant{Tools: list, Forward: forward}
		return nil
	}); err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Text(grantText(m.Name, Grant{Tools: list, Forward: forward}))
}

// grantText says what a member may do here, the way the user should hear it
// before trusting a machine with it.
func grantText(name string, g Grant) string {
	var b strings.Builder
	if len(g.Tools) == 0 {
		fmt.Fprintf(&b, "%s's tasks may use every tool here, including the shell — on a machine where Factor runs as root, that is the whole machine", name)
	} else {
		tl := slices.Clone(g.Tools)
		slices.Sort(tl)
		fmt.Fprintf(&b, "%s's tasks may use only: %s", name, strings.Join(tl, ", "))
	}
	if g.Forward {
		b.WriteString(", and may pass work on to other members.")
	} else {
		b.WriteString(", and may not pass work on to other members.")
	}
	return b.String()
}

// peer returns the record for id, making one if needed.
func (st *State) peer(id string) *Peer {
	if st.Peers == nil {
		st.Peers = map[string]*Peer{}
	}
	p, ok := st.Peers[id]
	if !ok || p == nil {
		p = &Peer{}
		st.Peers[id] = p
	}
	return p
}
