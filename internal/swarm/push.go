package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Push notifications ride the tunnel. A member has no listening port for a
// webhook, but it does have https://<agent-id>.swarm/, so a worker pushes a
// task's changes there and the caller hears of them at once instead of on its
// next poll. A push to any other address is refused: a task's caller must not
// be able to aim this agent at a URL of its choosing.

const pushPath = "swarm/v1/push"

// pushHeader carries the token the caller set, as the A2A push binding does.
const pushHeader = "A2A-Notification-Token"

// tunnelPusher implements the SDK's push sender over the tunnel.
type tunnelPusher struct{ s *Service }

// SendPush never fails the task it reports on: a push only saves the caller
// a poll, and a task must not end over a notification that did not land.
func (p tunnelPusher) SendPush(ctx context.Context, cfg *a2a.PushConfig, event a2a.Event) error {
	if err := p.send(ctx, cfg, event); err != nil {
		slog.Debug("swarm: a push did not land", "url", cfg.URL, "error", err)
	}
	return nil
}

func (p tunnelPusher) send(ctx context.Context, cfg *a2a.PushConfig, event a2a.Event) error {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("a push goes to a member's tunnel address, not %q", cfg.URL)
	}
	peer, ok := strings.CutSuffix(u.Hostname(), HostSuffix)
	if !ok || !p.s.IsMember(peer) || strings.TrimPrefix(u.Path, "/") != pushPath {
		return fmt.Errorf("a push goes to a member's tunnel address, not %q", cfg.URL)
	}
	transport, _, _ := p.s.net()
	if transport == nil {
		return errOffline
	}
	hc, base, err := transport.HTTPClient(peer)
	if err != nil {
		return err
	}
	body, err := json.Marshal(a2a.StreamResponse{Event: event})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, join(base, pushPath), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set(pushHeader, cfg.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the push was refused with status %d", resp.StatusCode)
	}
	return nil
}

// pushConfig is what a delegation asks the worker to push to.
func (s *Service) pushConfig(d *Delegation) *a2a.PushConfig {
	return &a2a.PushConfig{URL: "https://" + s.id.ID + HostSuffix + "/" + pushPath, Token: d.Token}
}

// servePush takes a worker's push and wakes whatever follows that task, which
// asks for the task at once rather than at its next poll. The push itself
// settles nothing: one owner settles each delegation, so a result is
// delivered once whichever way the news arrived.
func (s *Service) servePush(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.peerOf(r)
	if !ok {
		http.Error(w, "unknown agent", http.StatusUnauthorized)
		return
	}
	var sr a2a.StreamResponse
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&sr); err != nil {
		http.Error(w, "not an A2A event", http.StatusBadRequest)
		return
	}
	task := ""
	switch e := sr.Event.(type) {
	case *a2a.Task:
		task = string(e.ID)
	case *a2a.TaskStatusUpdateEvent:
		task = string(e.TaskID)
	case *a2a.TaskArtifactUpdateEvent:
		task = string(e.TaskID)
	}
	token := r.Header.Get(pushHeader)
	for _, d := range s.deleg.all() {
		if d.Peer == peer && d.Task == task && d.Token != "" && d.Token == token {
			s.nudge(d.ID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// follower registers a delegation as followed by this process, and returns
// the channel a push nudges it on and the function that lets go of it.
func (s *Service) follower(id string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.followMu.Lock()
	if s.followers == nil {
		s.followers = map[string]chan struct{}{}
	}
	s.followers[id] = ch
	s.followMu.Unlock()
	return ch, func() {
		s.followMu.Lock()
		if s.followers[id] == ch {
			delete(s.followers, id)
		}
		s.followMu.Unlock()
	}
}

func (s *Service) nudge(id string) {
	s.followMu.Lock()
	ch := s.followers[id]
	s.followMu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// join puts a path under a base URL whether or not it ends in a slash.
func join(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}
