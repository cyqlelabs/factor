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

type Telegram struct {
	apiBase string
	token   string
	allow   map[string]bool
	b       *bus.MessageBus
	client  *http.Client
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	offset  int64
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
	t.wg.Add(1)
	go t.pollLoop(ctx)
	return nil
}

func (t *Telegram) Stop() error {
	if t.cancel != nil {
		t.cancel()
	}
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
	body, err := json.Marshal(map[string]any{"chat_id": msg.ChatID, "text": msg.Content})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiBase+"/sendMessage", bytes.NewReader(body))
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
		return fmt.Errorf("telegram sendMessage failed: HTTP %d %s", resp.StatusCode, parsed.Description)
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
