// Package a2a is Factor's side of the A2A protocol: a server that runs tasks
// other agents send, the task store behind it, and the client helpers
// swarm_send uses. It is protocol only. Who may call, what they may do and
// how a turn runs are the swarm's to decide, through Runner.
package a2a

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// ExtensionURI names Factor's A2A extension. It carries who can hear the
// caller's answer and the chain of agents the work has passed through.
const ExtensionURI = "https://fctr.app/a2a/swarm/v1"

// MaxHops is how many agents a piece of work may pass through after the one
// that asked for it.
const MaxHops = 3

// Audience values the extension carries.
const (
	AudiencePrivate = "private"
	AudienceShared  = "shared"
)

// Meta is the extension's payload. Every field is a claim made by an agent
// the connection has already identified, so it is trusted only as far as the
// grant of that agent goes.
type Meta struct {
	// Audience is "private" when only the caller's user can hear the answer.
	// Anything else, including a missing extension, is treated as shared.
	Audience string `json:"audience,omitempty"`
	// Chain is the agents the work passed through before reaching this one,
	// in order, starting with the one a person asked.
	Chain []string `json:"chain,omitempty"`
	Hops  int      `json:"hops"`
}

// Private reports whether the caller vouched that its user is alone.
func (m Meta) Private() bool { return m.Audience == AudiencePrivate }

// Next is the extension a turn forwarding this work to a third agent sends:
// this agent appended to the chain, one hop further.
func (m Meta) Next(self string) Meta {
	return Meta{Audience: m.Audience, Chain: append(slices.Clone(m.Chain), self), Hops: m.Hops + 1}
}

// Loops reports whether sending this work to target would bring it back to
// an agent it already passed through.
func (m Meta) Loops(target string) bool {
	return slices.ContainsFunc(m.Chain, func(id string) bool { return strings.EqualFold(id, target) })
}

// Attach writes m onto a message.
func (m Meta) Attach(msg *a2a.Message) {
	msg.SetMeta(ExtensionURI, m)
	if !slices.Contains(msg.Extensions, ExtensionURI) {
		msg.Extensions = append(msg.Extensions, ExtensionURI)
	}
}

// MetaOf reads the extension off a message. ok is false when the message
// does not carry it, which a task forwarded without it is read as: no chain,
// and so no permission to pass the work on.
func MetaOf(msg *a2a.Message) (Meta, bool) {
	if msg == nil {
		return Meta{}, false
	}
	raw, present := msg.Metadata[ExtensionURI]
	if !present {
		return Meta{}, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Meta{}, false
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, false
	}
	return m, true
}

// TextOf joins the text parts of a message.
func TextOf(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	return partsText(msg.Parts)
}

func partsText(parts a2a.ContentParts) string {
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p.Content.(a2a.Text); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(string(t))
		}
	}
	return b.String()
}

// ResultText is what a finished task said: its artifacts, or failing those
// the message on its status.
func ResultText(task *a2a.Task) string {
	if task == nil {
		return ""
	}
	var out []string
	for _, art := range task.Artifacts {
		if t := partsText(art.Parts); t != "" {
			out = append(out, t)
		}
	}
	if len(out) > 0 {
		return strings.Join(out, "\n")
	}
	return TextOf(task.Status.Message)
}

// Terminal reports whether a task state is final.
func Terminal(s a2a.TaskState) bool {
	switch s {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	}
	return false
}
