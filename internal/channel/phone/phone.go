package phone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

func init() {
	channel.Register("phone", func(raw json.RawMessage, b *bus.MessageBus) (channel.Channel, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("phone config: %w", err)
		}
		return New(cfg, b)
	})
}

// Phone is the voice connector. Unlike the other channels it does not publish
// inbound turns onto the bus: a phone call is synchronous, so the bridge runs
// each turn directly and hands the reply straight back to the voice shell.
// The bus is still used for the one thing that is genuinely asynchronous — a
// finished outbound call reporting back to whoever asked for it.
type Phone struct {
	cfg     Config
	home    string
	token   string
	bridge  *bridge
	shell   *supervisor
	speech  *speechSupervisor // nil unless a local tier runs on Factor's own server
	carrier carrierClient

	// speechWait is how long the voice shell holds for the speech server to
	// load its models before giving up and letting the tier fall back.
	speechWait time.Duration

	cancel context.CancelFunc

	mu        sync.Mutex
	effective Config
}

// New builds the connector from an already-decoded config section.
func New(cfg Config, b *bus.MessageBus) (*Phone, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("phone: %w", err)
	}
	publish := func(bus.InboundMessage) bool { return false }
	if b != nil {
		publish = b.PublishInbound
	}
	p := &Phone{
		cfg:        cfg,
		home:       config.Home(),
		token:      newBridgeToken(),
		carrier:    newCarrierClient(cfg),
		effective:  cfg,
		speechWait: speechReadyTimeout,
	}
	p.bridge = newBridge(p.token, cfg.inboundAllowed, publish)
	p.shell = newSupervisor(cfg, p.home, p.token, p.shellConfig)
	if cfg.managedSpeech() {
		p.speech = newSpeechSupervisor(cfg.SpeechServer, p.home, cfg.Language, p.token,
			cfg.localSTT(), cfg.localTTS())
	}

	if cfg.allowAnyCaller() {
		slog.Warn("SECURITY: channels.phone.allow_from is \"*\" — anyone who dials this number reaches this agent; list the numbers instead")
	}
	return p, nil
}

// newBridgeToken mints the shared secret for this boot. It only ever travels
// from Factor to its own child process, through the environment.
func newBridgeToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail in practice; a time-derived token is still
		// better than an unauthenticated loopback endpoint.
		return fmt.Sprintf("factor-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (p *Phone) Name() string          { return "phone" }
func (p *Phone) MaxMessageLength() int { return maxMessageLength }

// BindTurnRunner attaches the agent loop. The gateway calls this after
// building channels; without it the bridge answers 503 and calls are refused
// rather than silently dropped.
func (p *Phone) BindTurnRunner(run channel.TurnFunc) { p.bridge.bindRunner(TurnFunc(run)) }

// Toolset are the tools that only exist where the phone does. They are
// registered by the gateway, so a CLI session never sees a tool it could not
// use.
func (p *Phone) Toolset() []tools.Tool {
	return []tools.Tool{
		&smsTool{phone: p},
		&callTool{phone: p},
	}
}

func (p *Phone) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)
	if err := p.bridge.listen(p.cfg.BridgePort); err != nil {
		p.cancel()
		return err
	}
	p.bridge.serve()
	// The speech server goes up first: the voice shell probes it as it starts,
	// and a probe that arrives before the models have loaded would read as an
	// unreachable server and quietly demote the call to the cloud tier.
	if p.speech != nil {
		p.speech.start(ctx)
	}
	p.shell.start(ctx)
	slog.Info("phone channel ready",
		"number", p.cfg.PhoneNumber, "tier", p.cfg.TierLabel(),
		"bridge_port", p.bridge.port(), "control_port", p.cfg.SidecarPort)
	return nil
}

func (p *Phone) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.shell.stop()
	if p.speech != nil {
		p.speech.stop()
	}
	p.bridge.shutdown()
	return nil
}

// shellConfig renders what the voice shell should run with, re-checking the
// local speech servers each time: one that was down at boot and has since come
// up is picked up on the next restart, with no config change.
func (p *Phone) shellConfig() (shellConfig, error) {
	// Loading a transcription model takes tens of seconds, so give Factor's own
	// speech server that long to answer before deciding the local tier is
	// unreachable. This runs on the shell supervisor's goroutine, which has
	// nothing else to do until the speech it depends on is up.
	if p.speech != nil {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), p.speechWait)
		if !p.speech.waitHealthy(waitCtx, p.speechWait) {
			slog.Warn("the local speech server is not ready yet", "reason", p.speech.Down())
		}
		waitCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	effective, err := resolveAudioTier(ctx, p.cfg)
	if err != nil {
		return shellConfig{}, err
	}
	p.mu.Lock()
	p.effective = effective
	p.mu.Unlock()
	return renderShellConfig(effective, p.bridge.port(), p.token), nil
}

// Tier reports the speech tier actually in force, which is the configured one
// unless a local server was unreachable and the cloud took over.
func (p *Phone) Tier() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.effective.TierLabel()
}

// Healthy reports whether the voice shell is answering; Down explains why not.
func (p *Phone) Healthy() bool { return p.shell.Healthy() }
func (p *Phone) Down() string  { return p.shell.Down() }

// Send delivers a bus message — a cron result, a finished job, anything the
// agent wants to say when the user is not on the line. How it arrives is the
// user's choice: a text (default), a phone call, or nothing at all.
func (p *Phone) Send(ctx context.Context, msg bus.OutboundMessage) error {
	to := p.destination(msg.ChatID)
	if !p.cfg.outboundAllowed(to) {
		slog.Warn("refusing to reach a number outside the outbound allowlist", "to", to)
		return fmt.Errorf("%s is not on the outbound allowlist", to)
	}
	switch p.cfg.Proactive {
	case proactiveOff:
		slog.Warn("dropping a proactive message: channels.phone.proactive is \"off\"",
			"to", to, "chars", len(msg.Content))
		return nil
	case proactiveCall:
		_, err := p.placeCall(ctx, to, "deliver this message and answer any follow-up", msg.Content, origin{})
		if err == nil {
			return nil
		}
		// A call that cannot even be placed is worth an SMS rather than
		// silence; the manager's own retries would otherwise re-dial.
		slog.Error("proactive call failed; falling back to SMS", "to", to, "error", err)
		return p.sendSMS(ctx, to, msg.Content)
	default:
		return p.sendSMS(ctx, to, msg.Content)
	}
}

// destination resolves who a message is for: the session's own number when it
// is one (a call's session key is the caller's number), else the owner.
func (p *Phone) destination(chatID string) string {
	if n := normalizeNumber(chatID); validNumber(n) {
		return n
	}
	return p.cfg.UserNumber
}

func (p *Phone) sendSMS(ctx context.Context, to, body string) error {
	id, err := p.carrier.sendSMS(ctx, p.cfg.PhoneNumber, to, body)
	if err != nil {
		return err
	}
	slog.Info("sms sent", "to", to, "id", id)
	return nil
}

// placeCall asks the voice shell to dial and records where the outcome should
// be reported. It returns as soon as the call is queued.
func (p *Phone) placeCall(ctx context.Context, to, goal, firstMessage string, from origin) (string, error) {
	callID, err := p.shell.control.placeCall(ctx, placeCallRequest{
		To:           to,
		Goal:         goal,
		FirstMessage: firstMessage,
	})
	if err != nil {
		return "", err
	}
	p.bridge.registerOutbound(callID, to, goal, from)
	slog.Info("outbound call placed", "call", callID, "to", to)
	return callID, nil
}

// redact keeps carrier credentials out of anything a user or the model sees.
func (p *Phone) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, secret := range []string{p.cfg.TwilioAuthToken, p.cfg.TelnyxAPIKey,
		p.cfg.ElevenLabsAPIKey, p.cfg.STTAPIKey, p.token} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return fmt.Errorf("%s", msg)
}
