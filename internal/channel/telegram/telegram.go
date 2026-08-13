// Package telegram is the reference connector: raw Bot API over HTTP
// long-polling, no SDK. Config section:
//
//	"channels": {"telegram": {"token": "...", "allow_from": ["12345", "@name"]}}
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
)

func init() {
	channel.Register("telegram", func(raw json.RawMessage, b *bus.MessageBus) (channel.Channel, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("telegram config: %w", err)
		}
		return New(cfg, b)
	})
}

type Config struct {
	Enabled   *bool    `json:"enabled"`
	Token     string   `json:"token"`
	AllowFrom []string `json:"allow_from"`
	APIBase   string   `json:"api_base"` // override for tests; default api.telegram.org
}

// typingInterval sits under Telegram's ~5s chat-action lifetime, so a
// re-sent action keeps the indicator unbroken for as long as a turn runs.
var typingInterval = 4 * time.Second

type Telegram struct {
	apiBase string
	token   string
	allow   map[string]bool
	b       *bus.MessageBus
	client  *http.Client
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	offset  int64

	typingMu sync.Mutex
	runCtx   context.Context // set by Start; parents the typing loops
	typing   map[string]context.CancelFunc
}

// redact keeps the bot token out of error chains: transport errors embed the
// full request URL, which contains the token, and errors end up in logs.
func (t *Telegram) redact(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), t.token, "[token]"))
}

func New(cfg Config, b *bus.MessageBus) (*Telegram, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram: token is required")
	}
	base := cfg.APIBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	t := &Telegram{
		apiBase: base + "/bot" + cfg.Token,
		token:   cfg.Token,
		allow:   map[string]bool{},
		b:       b,
		client:  &http.Client{Timeout: 70 * time.Second},
		typing:  map[string]context.CancelFunc{},
	}
	for _, a := range cfg.AllowFrom {
		t.allow[strings.TrimSpace(a)] = true
	}
	if len(t.allow) == 0 {
		slog.Warn("SECURITY: telegram allow_from is empty — every Telegram user can talk to this agent; set channels.telegram.allow_from")
	}
	return t, nil
}

func (t *Telegram) Name() string          { return "telegram" }
func (t *Telegram) MaxMessageLength() int { return 4000 }

func (t *Telegram) Start(ctx context.Context) error {
	ctx, t.cancel = context.WithCancel(ctx)
	t.typingMu.Lock()
	t.runCtx = ctx
	t.typingMu.Unlock()
	t.wg.Add(1)
	go t.pollLoop(ctx)
	return nil
}

func (t *Telegram) Stop() error {
	if t.cancel != nil {
		t.cancel()
	}
	// Turns outlive the connector during shutdown and keep reporting phases;
	// dropping the run context makes those late calls inert instead of
	// racing a new typing goroutine against the wait below.
	t.typingMu.Lock()
	t.runCtx = nil
	t.typingMu.Unlock()
	t.wg.Wait()
	return nil
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		From *struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

func (t *Telegram) pollLoop(ctx context.Context) {
	defer t.wg.Done()
	for ctx.Err() == nil {
		updates, err := t.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("telegram poll failed", "error", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= t.offset {
				t.offset = u.UpdateID + 1
			}
			t.handle(u)
		}
	}
}

func (t *Telegram) getUpdates(ctx context.Context) ([]update, error) {
	url := fmt.Sprintf("%s/getUpdates?timeout=50&offset=%d&allowed_updates=[\"message\"]", t.apiBase, t.offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, t.redact(err) // the URL in this error embeds the token
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.redact(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, t.redact(err)
	}
	var parsed struct {
		OK          bool     `json:"ok"`
		Result      []update `json:"result"`
		Description string   `json:"description"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if !parsed.OK {
		return nil, fmt.Errorf("telegram API: %s", parsed.Description)
	}
	return parsed.Result, nil
}

func (t *Telegram) allowed(id int64, username string) bool {
	if len(t.allow) == 0 {
		return true
	}
	return t.allow[strconv.FormatInt(id, 10)] || (username != "" && t.allow["@"+username])
}

func (t *Telegram) handle(u update) {
	if u.Message == nil || u.Message.Text == "" || u.Message.From == nil {
		return
	}
	if !t.allowed(u.Message.From.ID, u.Message.From.Username) {
		slog.Warn("telegram message from non-allowed sender dropped",
			"sender", u.Message.From.ID, "username", u.Message.From.Username)
		return
	}
	t.b.PublishInbound(bus.InboundMessage{
		Channel:  "telegram",
		SenderID: strconv.FormatInt(u.Message.From.ID, 10),
		ChatID:   strconv.FormatInt(u.Message.Chat.ID, 10),
		Content:  u.Message.Text,
		Time:     time.Now(),
	})
}

func (t *Telegram) Send(ctx context.Context, msg bus.OutboundMessage) error {
	return t.call(ctx, "sendMessage", map[string]any{"chat_id": msg.ChatID, "text": msg.Content})
}

// SetTyping starts or stops the "typing…" indicator for one chat. The agent
// loop calls it from turn goroutines, so it only touches the map here and
// leaves the API calls to a per-chat goroutine.
func (t *Telegram) SetTyping(chatID string, on bool) {
	t.typingMu.Lock()
	defer t.typingMu.Unlock()
	if !on {
		if cancel, running := t.typing[chatID]; running {
			cancel()
			delete(t.typing, chatID)
		}
		return
	}
	if _, running := t.typing[chatID]; running || t.runCtx == nil {
		return
	}
	ctx, cancel := context.WithCancel(t.runCtx)
	t.typing[chatID] = cancel
	t.wg.Add(1)
	go t.typingLoop(ctx, chatID)
}

// typingLoop re-sends the chat action until the turn ends or the connector
// stops. Telegram clears the indicator on its own once the reply lands.
func (t *Telegram) typingLoop(ctx context.Context, chatID string) {
	defer t.wg.Done()
	for {
		if err := t.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}); err != nil && ctx.Err() == nil {
			slog.Debug("telegram sendChatAction failed", "chat", chatID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(typingInterval):
		}
	}
}

// call posts a JSON payload to one Bot API method and checks its ok flag.
func (t *Telegram) call(ctx context.Context, method string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiBase+"/"+method, bytes.NewReader(body))
	if err != nil {
		return t.redact(err) // the URL in this error embeds the token
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return t.redact(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || !parsed.OK {
		return fmt.Errorf("telegram %s failed: HTTP %d %s", method, resp.StatusCode, parsed.Description)
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
