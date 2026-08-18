package phone

import (
	"context"
	"time"
)

// The managed speech server knows nothing about telephony, and the PC voice
// channel runs the same engines against the same weights. This is the surface
// it borrows: the supervisor, the endpoint, and the startup probe — exported
// here so the speech stack keeps one owner instead of growing a second copy.

// SpeechServer supervises one managed local speech server for a channel other
// than the phone: spawn, health-poll, restart with backoff, install on demand.
type SpeechServer struct {
	inner *speechSupervisor
}

// NewSpeechServer builds a supervisor for the server described by cfg. The
// token is the caller's boot secret; needSTT and needTTS decide which weights
// the server loads.
func NewSpeechServer(cfg SpeechConfig, home, language, token string, needSTT, needTTS bool) *SpeechServer {
	return &SpeechServer{inner: newSpeechSupervisor(cfg, home, language, token, needSTT, needTTS)}
}

// Start begins supervising; it returns immediately.
func (s *SpeechServer) Start(ctx context.Context) { s.inner.start(ctx) }

// Stop shuts the server down and waits for the supervisor to exit.
func (s *SpeechServer) Stop() { s.inner.stop() }

// Healthy reports whether the server is answering; Down explains why not.
func (s *SpeechServer) Healthy() bool { return s.inner.Healthy() }
func (s *SpeechServer) Down() string  { return s.inner.Down() }

// WaitHealthy blocks until the server answers or the timeout passes — model
// loading takes tens of seconds, and probing earlier reads as an outage.
func (s *SpeechServer) WaitHealthy(ctx context.Context, timeout time.Duration) bool {
	return s.inner.waitHealthy(ctx, timeout)
}

// SetProbeInterval shortens the health-poll cadence. It exists for tests in
// other packages, which cannot reach the supervisor's unexported field.
func (s *SpeechServer) SetProbeInterval(d time.Duration) { s.inner.probeInterval = d }

// SpeechBaseURL is the endpoint the managed server answers on for a given
// configuration — what an empty base_url on a local tier resolves to.
func SpeechBaseURL(cfg SpeechConfig) string { return speechBaseURL(cfg) }

// ProbeSpeechServer reports whether an OpenAI-compatible speech server is
// answering at baseURL. Any HTTP response counts.
func ProbeSpeechServer(ctx context.Context, baseURL string) error {
	return probeAudioServer(ctx, baseURL)
}
