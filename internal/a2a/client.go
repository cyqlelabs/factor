package a2a

import (
	"context"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// Dial returns a client for the agent at baseURL, speaking JSON-RPC over hc.
// The swarm's tunnel supplies hc, so the identity behind the connection is
// the pinned one and not whatever answers at the address.
func Dial(ctx context.Context, hc *http.Client, baseURL string) (*a2aclient.Client, error) {
	return a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{a2a.NewAgentInterface(baseURL, a2a.TransportProtocolJSONRPC)},
		a2aclient.WithJSONRPCTransport(hc))
}

// Card describes this agent to the others.
func Card(name, version, url string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:                name,
		Description:         "A Factor agent: a personal desktop agent with long-term memory, reachable by the other agents in its swarm.",
		Version:             version,
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)},
		Capabilities: a2a.AgentCapabilities{
			Streaming: true,
			Extensions: []a2a.AgentExtension{{URI: ExtensionURI,
				Description: "Who can hear the caller's answer, and the agents the work has passed through."}},
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{ID: "task", Name: "Do a task",
			Description: "Carry out a request on this machine with its tools and memory, and report the result.",
			Tags:        []string{"general"}}},
	}
}
