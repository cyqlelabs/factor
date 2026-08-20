package phone

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	"github.com/cyqlelabs/factor/internal/config"
)

// fakeTwilio stands in for the carrier's REST API and records what it was
// asked to send.
type fakeTwilio struct {
	*httptest.Server
	mu       sync.Mutex
	messages []url.Values
	status   int
	body     string
}

func newFakeTwilio(t *testing.T) *fakeTwilio {
	t.Helper()
	f := &fakeTwilio{status: http.StatusCreated, body: `{"sid":"SM123","status":"queued"}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user == "" || pass == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.messages = append(f.messages, r.PostForm)
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTwilio) sent() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values{}, f.messages...)
}

func (f *fakeTwilio) fail(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// fakeShellAPI stands in for the voice shell's control API.
type fakeShellAPI struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []placeCallRequest
	refuse bool
}

func newFakeShellAPI(t *testing.T) *fakeShellAPI {
	t.Helper()
	f := &fakeShellAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		var req placeCallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.calls = append(f.calls, req)
		refuse := f.refuse
		f.mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"the carrier is down"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"call_id": "CA-out-1"})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeShellAPI) placed() []placeCallRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]placeCallRequest{}, f.calls...)
}

// newTestPhone builds a connector wired to fakes, in a throwaway home.
func newTestPhone(t *testing.T, mutate func(*Config)) (*Phone, *fakeTwilio, *fakeShellAPI) {
	t.Helper()
	t.Setenv("FACTOR_HOME", t.TempDir())
	twilio, shell := newFakeTwilio(t), newFakeShellAPI(t)

	cfg := validConfig()
	cfg.APIBase = twilio.URL
	cfg.ControlAPIBase = shell.URL
	cfg.SidecarPort = freePort(t)
	cfg.BridgePort = freePort(t)
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg, bus.New())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, twilio, shell
}

func TestRegisteredFactoryBuildsPhoneFromAConfigSection(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	section := json.RawMessage(`{
		"user_number": "+15550001111",
		"phone_number": "+15550002222",
		"twilio_account_sid": "AC1",
		"twilio_auth_token": "twilio-secret",
		"elevenlabs_api_key": "eleven-secret",
		"stt_api_key": "deepgram-secret"
	}`)
	built := channel.Build(map[string]json.RawMessage{"phone": section}, bus.New())
	if len(built) != 1 {
		t.Fatalf("channel.Build produced %d channels, want 1", len(built))
	}
	if built[0].Name() != "phone" {
		t.Errorf("name = %q", built[0].Name())
	}
	if got := built[0].MaxMessageLength(); got != maxMessageLength {
		t.Errorf("MaxMessageLength = %d, want %d (SMS-safe chunks)", got, maxMessageLength)
	}
	// The connector runs turns itself and brings its own tools.
	if _, ok := built[0].(channel.TurnRunner); !ok {
		t.Error("the phone connector does not implement channel.TurnRunner")
	}
	if _, ok := built[0].(channel.Toolset); !ok {
		t.Error("the phone connector does not implement channel.Toolset")
	}
}

func TestBuildSkipsDisabledAndBrokenSections(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	cases := map[string]json.RawMessage{
		"disabled":  json.RawMessage(`{"enabled":false,"user_number":"+15550001111"}`),
		"invalid":   json.RawMessage(`{"user_number":"not-a-number"}`),
		"malformed": json.RawMessage(`{"user_number":`),
	}
	for name, section := range cases {
		t.Run(name, func(t *testing.T) {
			if built := channel.Build(map[string]json.RawMessage{"phone": section}, bus.New()); len(built) != 0 {
				t.Errorf("channel.Build produced %d channels, want none", len(built))
			}
		})
	}
}

// Every credential in the section has to reach the secret filter, or a tool
// result could print the carrier token back to the model.
func TestSecretsInThePhoneSectionAreScrubbed(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = map[string]json.RawMessage{
		"phone": json.RawMessage(`{
			"user_number": "+15550001111",
			"twilio_auth_token": "twilio-secret",
			"telnyx_api_key": "telnyx-secret",
			"elevenlabs_api_key": "eleven-secret",
			"stt_api_key": "deepgram-secret"
		}`),
	}
	secrets := strings.Join(cfg.SecretValues(), " ")
	for _, want := range []string{"twilio-secret", "telnyx-secret", "eleven-secret", "deepgram-secret"} {
		if !strings.Contains(secrets, want) {
			t.Errorf("SecretValues() is missing %q — it would not be redacted from tool output", want)
		}
	}
	filtered := cfg.FilterSecrets("token=twilio-secret carrier=telnyx-secret key=eleven-secret stt=deepgram-secret")
	if strings.Contains(filtered, "secret") {
		t.Errorf("FilterSecrets left credentials behind: %q", filtered)
	}
	// The owner's number is not a secret and must survive.
	if !strings.Contains(cfg.FilterSecrets("call +15550001111"), "+15550001111") {
		t.Error("the owner's number was redacted")
	}
}

func TestNewRejectsAnInvalidSection(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	if _, err := New(Config{}, bus.New()); err == nil {
		t.Fatal("New accepted an empty section")
	} else if !strings.HasPrefix(err.Error(), "phone:") {
		t.Errorf("error %q is not attributed to the phone channel", err)
	}
}

func TestStartAndStopAreClean(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p.bridge.port() == 0 {
		t.Error("the bridge did not bind")
	}
	if got := p.Tier(); got != "tier 1 · cloud audio" {
		t.Errorf("Tier() = %q", got)
	}
	if err := p.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// A cloud tier runs no speech server, and the shell config is rendered without
// waiting on one.
func TestShellConfigOnTheCloudTier(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)
	if p.speech != nil {
		t.Fatal("a cloud tier should not supervise a speech server")
	}
	shell, err := p.shellConfig()
	if err != nil {
		t.Fatalf("shellConfig: %v", err)
	}
	if shell.Tier != 1 || shell.STT.Provider != providerDeepgram {
		t.Errorf("shell config = tier %d / %+v", shell.Tier, shell.STT)
	}
}

// A managed local tier whose server never comes up must not hang the gateway:
// it degrades to the cloud tier and says so, rather than failing every call.
func TestShellConfigFallsBackWhenTheSpeechServerNeverStarts(t *testing.T) {
	p, _, _ := newTestPhone(t, func(c *Config) {
		c.STT.Provider = providerLocalOpenAI
		c.SpeechServer.Port = freePort(t)
	})
	if p.speech == nil {
		t.Fatal("a managed local tier should supervise a speech server")
	}
	// Nothing is listening and nothing will start: the wait has to give up.
	p.speech.probeInterval = 20 * time.Millisecond
	p.speechWait = 200 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		shell, err := p.shellConfig()
		if err != nil {
			t.Errorf("shellConfig: %v", err)
			return
		}
		if shell.STT.Provider != providerDeepgram {
			t.Errorf("stt = %+v, want the cloud fallback", shell.STT)
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("shellConfig never returned")
	}
}

func TestStartReportsAPortClash(t *testing.T) {
	taken := freePort(t)
	first, _, _ := newTestPhone(t, func(c *Config) { c.BridgePort = taken })
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() { _ = first.Stop() }()

	second, _, _ := newTestPhone(t, func(c *Config) { c.BridgePort = taken })
	if err := second.Start(context.Background()); err == nil {
		_ = second.Stop()
		t.Fatal("a second connector bound a port that was already taken")
	}
}

func TestBindTurnRunnerReachesTheBridge(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)
	var got string
	p.BindTurnRunner(func(_ context.Context, content, sessionKey string, _ func(string)) (string, error) {
		got = sessionKey
		return "ok:" + content, nil
	})
	reply, err := p.bridge.runner()(context.Background(), "hello", "phone:+15550001111", func(string) {})
	if err != nil || reply != "ok:hello" {
		t.Errorf("runner = %q, %v", reply, err)
	}
	if got != "phone:+15550001111" {
		t.Errorf("session key = %q", got)
	}
}

// ---- proactive delivery ----------------------------------------------------

func TestSendTextsByDefault(t *testing.T) {
	p, twilio, shell := newTestPhone(t, nil)
	err := p.Send(context.Background(), bus.OutboundMessage{
		Channel: "phone", ChatID: "+15550001111", Content: "the backup finished",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := twilio.sent()
	if len(sent) != 1 {
		t.Fatalf("twilio received %d messages, want 1", len(sent))
	}
	if got := sent[0].Get("To"); got != "+15550001111" {
		t.Errorf("To = %q", got)
	}
	if got := sent[0].Get("From"); got != "+15550002222" {
		t.Errorf("From = %q, want the number bought at the carrier", got)
	}
	if got := sent[0].Get("Body"); got != "the backup finished" {
		t.Errorf("Body = %q", got)
	}
	if len(shell.placed()) != 0 {
		t.Error("a text message placed a phone call")
	}
}

func TestSendFallsBackToTheOwnerWhenTheSessionIsNotANumber(t *testing.T) {
	p, twilio, _ := newTestPhone(t, nil)
	// A cron job created from the CLI has no phone number in its session key.
	if err := p.Send(context.Background(), bus.OutboundMessage{Channel: "phone", ChatID: "main", Content: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := twilio.sent()[0].Get("To"); got != "+15550001111" {
		t.Errorf("To = %q, want the owner", got)
	}
}

func TestSendCallsWhenAskedTo(t *testing.T) {
	p, twilio, shell := newTestPhone(t, func(c *Config) { c.Proactive = proactiveCall })
	if err := p.Send(context.Background(), bus.OutboundMessage{
		Channel: "phone", ChatID: "+15550001111", Content: "your flight is delayed",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	placed := shell.placed()
	if len(placed) != 1 {
		t.Fatalf("the shell was asked for %d calls, want 1", len(placed))
	}
	if placed[0].To != "+15550001111" || placed[0].FirstMessage != "your flight is delayed" {
		t.Errorf("call = %+v", placed[0])
	}
	if len(twilio.sent()) != 0 {
		t.Error("a successful call also sent a text")
	}
}

// A call that cannot even be placed is worth a text rather than silence.
func TestSendFallsBackToSMSWhenACallCannotBePlaced(t *testing.T) {
	p, twilio, shell := newTestPhone(t, func(c *Config) { c.Proactive = proactiveCall })
	shell.refuse = true

	if err := p.Send(context.Background(), bus.OutboundMessage{
		Channel: "phone", ChatID: "+15550001111", Content: "your flight is delayed",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(twilio.sent()) != 1 {
		t.Errorf("the message was lost: twilio saw %d texts", len(twilio.sent()))
	}
}

func TestSendOffDropsQuietlyWithoutFailingTheManager(t *testing.T) {
	p, twilio, shell := newTestPhone(t, func(c *Config) { c.Proactive = proactiveOff })
	if err := p.Send(context.Background(), bus.OutboundMessage{Channel: "phone", ChatID: "+15550001111", Content: "hi"}); err != nil {
		t.Errorf("Send returned %v; \"off\" must not make the manager retry", err)
	}
	if len(twilio.sent()) != 0 || len(shell.placed()) != 0 {
		t.Error("\"off\" still reached out")
	}
}

func TestSendIgnoresInProgressNotes(t *testing.T) {
	p, twilio, shell := newTestPhone(t, nil)
	err := p.Send(context.Background(), bus.OutboundMessage{
		Channel: "phone", ChatID: "+15550001111", Content: "Looking that up now.", Interim: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(twilio.sent()) != 0 || len(shell.placed()) != 0 {
		t.Error("an in-progress note reached the carrier; only the answer is worth a text or a call")
	}
}

func TestSendRefusesNumbersOutsideTheOutboundAllowlist(t *testing.T) {
	p, twilio, _ := newTestPhone(t, nil)
	err := p.Send(context.Background(), bus.OutboundMessage{Channel: "phone", ChatID: "+15559999999", Content: "hi"})
	if err == nil {
		t.Fatal("Send reached a number that is not on the outbound allowlist")
	}
	if len(twilio.sent()) != 0 {
		t.Error("a refused destination still hit the carrier")
	}
}

func TestSendSurfacesCarrierRejections(t *testing.T) {
	p, twilio, _ := newTestPhone(t, nil)
	twilio.fail(http.StatusBadRequest, `{"message":"The 'To' number is not a valid mobile number","code":21614}`)

	err := p.Send(context.Background(), bus.OutboundMessage{Channel: "phone", ChatID: "+15550001111", Content: "hi"})
	if err == nil {
		t.Fatal("a rejected message was reported as sent")
	}
	if !strings.Contains(err.Error(), "not a valid mobile number") {
		t.Errorf("error = %q, want the carrier's own message", err)
	}
	if strings.Contains(err.Error(), "twilio-secret") {
		t.Errorf("the carrier token leaked into an error: %q", err)
	}
}

// ---- the other carrier -----------------------------------------------------

// fakeTelnyx stands in for Telnyx's REST API, which speaks JSON and a bearer
// token where Twilio speaks form posts and basic auth.
type fakeTelnyx struct {
	*httptest.Server
	mu       sync.Mutex
	messages []map[string]string
	auth     []string
	status   int
	body     string
}

func newFakeTelnyx(t *testing.T) *fakeTelnyx {
	t.Helper()
	f := &fakeTelnyx{status: http.StatusOK, body: `{"data":{"id":"msg-1","record_type":"message"}}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var msg map[string]string
		_ = json.NewDecoder(r.Body).Decode(&msg)
		f.mu.Lock()
		f.messages = append(f.messages, msg)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelnyx) sent() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string{}, f.messages...)
}

func (f *fakeTelnyx) sentAuth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.auth...)
}

func (f *fakeTelnyx) fail(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// newTelnyxPhone is newTestPhone against the other carrier.
func newTelnyxPhone(t *testing.T) (*Phone, *fakeTelnyx) {
	t.Helper()
	t.Setenv("FACTOR_HOME", t.TempDir())
	carrier := newFakeTelnyx(t)

	cfg := validConfig()
	telnyxConfig(&cfg)
	cfg.APIBase = carrier.URL
	cfg.ControlAPIBase = newFakeShellAPI(t).URL
	cfg.SidecarPort = freePort(t)
	cfg.BridgePort = freePort(t)
	p, err := New(cfg, bus.New())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, carrier
}

func TestSendTextsThroughTelnyx(t *testing.T) {
	p, carrier := newTelnyxPhone(t)
	if err := p.Send(context.Background(), bus.OutboundMessage{
		Channel: "phone", ChatID: "+15550001111", Content: "the backup finished",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := carrier.sent()
	if len(sent) != 1 {
		t.Fatalf("telnyx received %d messages, want 1", len(sent))
	}
	want := map[string]string{"to": "+15550001111", "from": "+15550002222", "text": "the backup finished"}
	for key, value := range want {
		if sent[0][key] != value {
			t.Errorf("%s = %q, want %q", key, sent[0][key], value)
		}
	}
	if got := carrier.sentAuth()[0]; got != "Bearer telnyx-secret" {
		t.Errorf("Authorization = %q, want a bearer token", got)
	}
}

func TestTelnyxRejectionsSurfaceWithoutTheKey(t *testing.T) {
	p, carrier := newTelnyxPhone(t)
	carrier.fail(http.StatusUnprocessableEntity,
		`{"errors":[{"title":"Invalid destination","detail":"to is not a valid phone number"}]}`)

	err := p.Send(context.Background(), bus.OutboundMessage{Channel: "phone", ChatID: "+15550001111", Content: "hi"})
	if err == nil {
		t.Fatal("a rejected message was reported as sent")
	}
	if !strings.Contains(err.Error(), "to is not a valid phone number") {
		t.Errorf("error = %q, want the carrier's own detail", err)
	}
	if strings.Contains(p.redact(err).Error(), "telnyx-secret") {
		t.Errorf("the carrier key leaked into an error: %q", err)
	}
}

// Whatever the carrier answers, a message that did not go out has to come back
// as an error rather than a message id.
func TestTelnyxSMSFailureModes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"an error with only a title", http.StatusForbidden, `{"errors":[{"title":"Number not owned"}]}`, "Number not owned"},
		{"an answer that is not json", http.StatusBadGateway, `<html>gateway down</html>`, "gateway down"},
		{"an empty error array", http.StatusBadRequest, `{"errors":[]}`, "HTTP 400"},
		{"accepted but unidentified", http.StatusOK, `{"data":{}}`, "returned no id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			client := newTelnyxClient(Config{Carrier: carrierTelnyx, APIBase: srv.URL, TelnyxAPIKey: "telnyx-secret"})
			_, err := client.sendSMS(context.Background(), "+15550002222", "+15550001111", "hi")
			if err == nil {
				t.Fatal("a message that never went out was reported as sent")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

func TestRedactCoversEveryCredential(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)
	err := p.redact(fmt.Errorf("failed with twilio-secret / eleven-secret / deepgram-secret / %s", p.token))
	for _, secret := range []string{"twilio-secret", "eleven-secret", "deepgram-secret", p.token} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("redact left %q in %q", secret, err)
		}
	}
	if p.redact(nil) != nil {
		t.Error("redact(nil) should stay nil")
	}
}

func TestDestinationPrefersTheSessionsOwnNumber(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)
	for chatID, want := range map[string]string{
		"+15550003333":      "+15550003333",
		"+1 (555) 000-3333": "+15550003333",
		"main":              "+15550001111",
		"":                  "+15550001111",
	} {
		if got := p.destination(chatID); got != want {
			t.Errorf("destination(%q) = %q, want %q", chatID, got, want)
		}
	}
}

// A call Factor places has to be remembered, or its outcome has nowhere to go.
func TestPlaceCallRemembersWhereToReportBack(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)
	id, err := p.placeCall(context.Background(), "+15550001111", "ask about dinner", "hi",
		origin{Channel: "telegram", ChatID: "42"})
	if err != nil {
		t.Fatalf("placeCall: %v", err)
	}
	if id != "CA-out-1" {
		t.Errorf("call id = %q", id)
	}
	p.bridge.mu.Lock()
	info := p.bridge.calls[id]
	p.bridge.mu.Unlock()
	if info == nil || info.Origin.Channel != "telegram" || info.Origin.ChatID != "42" {
		t.Fatalf("the call's origin was not recorded: %+v", info)
	}
	if info.Goal != "ask about dinner" || info.Direction != directionOutbound {
		t.Errorf("call info = %+v", info)
	}
}

// The whole point of the connector: a cron result addressed to a phone session
// arrives as a text, through the manager's own chunking and retries.
func TestManagerDeliversAProactiveMessageAsSMS(t *testing.T) {
	p, twilio, _ := newTestPhone(t, nil)
	b := bus.New()
	manager := channel.NewManager(b, []channel.Channel{p})

	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	// Order matters: the manager's outbound pump only exits when the context
	// is done, so Stop would block forever if it ran first.
	defer func() {
		cancel()
		manager.Stop()
	}()

	b.PublishOutbound(bus.OutboundMessage{Channel: "phone", ChatID: "+15550001111", Content: "the report is ready"})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(twilio.sent()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := twilio.sent()
	if len(sent) == 0 {
		t.Fatal("the message never reached the carrier")
	}
	if got := sent[0].Get("Body"); got != "the report is ready" {
		t.Errorf("Body = %q", got)
	}
}

// Healthy and Down are what `factor status` and the tray row read: the phone
// must answer for the speech shell it depends on, not for itself. A channel
// that reports healthy while its shell is dead sends the user to look for the
// fault everywhere except where it is.
func TestPhoneHealthFollowsTheSpeechShell(t *testing.T) {
	p, _, _ := newTestPhone(t, nil)

	// Nothing has probed the shell yet, so nothing is claimed for it.
	if p.Healthy() {
		t.Error("the phone reported healthy before its shell was ever probed")
	}

	p.shell.healthy.Store(true)
	p.shell.down.Store("")
	if !p.Healthy() || p.Down() != "" {
		t.Errorf("with a live shell: Healthy=%v Down=%q", p.Healthy(), p.Down())
	}

	p.shell.healthy.Store(false)
	p.shell.setDown("the speech process exited (%s)", "signal: killed")
	if p.Healthy() {
		t.Error("the phone still reports healthy with a dead shell")
	}
	if !strings.Contains(p.Down(), "signal: killed") {
		t.Errorf("Down() = %q; it does not carry the shell's reason", p.Down())
	}
}
