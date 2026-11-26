package swarm

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/cyqlelabs/factor/internal/config"
)

// Agents outside the swarm are reached over plain HTTPS, with the server's
// certificate checked the ordinary way. They are named in swarm.agents, never
// learned, and the grant there is the whole of what Factor may tell them:
// only a turn the user started reaches one, and from a private conversation
// only when that agent is granted private.

const externalPrefix = "ext:"

func externalID(name string) string { return externalPrefix + name }

func isExternal(peer string) bool { return strings.HasPrefix(peer, externalPrefix) }

// external returns the configured agent behind an ext: id.
func (s *Service) external(peer string) (config.ExternalAgent, bool) {
	a, ok := s.cfg.Agents[strings.TrimPrefix(peer, externalPrefix)]
	return a, ok && isExternal(peer)
}

// resolve finds who a swarm_send is for: a member, or an agent outside.
func (s *Service) resolve(who string) (Member, error) {
	st := s.book.Read()
	m, err := st.Resolve(s.id.ID, who)
	if err == nil {
		return m, nil
	}
	for name := range s.cfg.Agents {
		if strings.EqualFold(name, strings.TrimSpace(who)) {
			return Member{ID: externalID(name), Name: name, Role: "external"}, nil
		}
	}
	if !st.Joined() && len(s.cfg.Agents) == 0 {
		return Member{}, errors.New("this agent is not in a swarm; the user can add it with the swarm tool")
	}
	return Member{}, err
}

// bearer adds a token to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// clientFor is the client and address that reach a peer: the tunnel for a
// member, plain HTTPS for an agent outside.
func (s *Service) clientFor(peer string) (*http.Client, string, error) {
	if !isExternal(peer) {
		transport, _, _ := s.net()
		if transport == nil {
			return nil, "", errOffline
		}
		return transport.HTTPClient(peer)
	}
	a, ok := s.external(peer)
	if !ok {
		return nil, "", fmt.Errorf("%s is not a configured agent", strings.TrimPrefix(peer, externalPrefix))
	}
	u, err := url.Parse(a.URL)
	plainLocal := u != nil && u.Scheme == "http" && loopbackHost(u.Hostname())
	if err != nil || (u.Scheme != "https" && !plainLocal) {
		return nil, "", fmt.Errorf("an agent outside the swarm is reached over https, not %q", a.URL)
	}
	hc := &http.Client{Transport: http.DefaultTransport}
	if a.Token != "" {
		hc.Transport = bearer{token: a.Token, next: http.DefaultTransport}
	}
	return hc, a.URL, nil
}

func loopbackHost(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// externalNames lists the agents outside, for the member list.
func (s *Service) externalNames() []string {
	var out []string
	for name, a := range s.cfg.Agents {
		grant := "requests from conversations with company only"
		if a.Private {
			grant = "requests from private conversations too"
		}
		out = append(out, fmt.Sprintf("%s (outside the swarm, %s): %s", name, a.URL, grant))
	}
	sort.Strings(out)
	return out
}
