package wizard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
)

// The wizard verifies what it configures instead of trusting it: a wrong key
// or a mistyped model name should surface here, in five seconds, and not
// three days later inside a failing turn.

const probeTimeout = 20 * time.Second

// ListModels asks an endpoint what models it serves. Not every
// OpenAI-compatible gateway implements /models, so an error here is
// informational — the caller falls back to free-text entry.
func ListModels(ctx context.Context, client *http.Client, cand config.Candidate) ([]string, error) {
	base := effectiveBase(cand)
	if base == "" {
		return nil, fmt.Errorf("no API base URL")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if cand.Type == "anthropic" {
		req.Header.Set("x-api-key", cand.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if cand.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cand.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s unreachable: %w", host(base), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, body)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unexpected /models response")
	}
	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	sort.Strings(models)
	return models, nil
}

// CheckProvider verifies credentials and model with one minimal completion.
func CheckProvider(ctx context.Context, cand config.Candidate) error {
	p, err := provider.New(cand)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, err = p.Chat(ctx, &provider.Request{
		Messages:  []provider.Message{{Role: "user", Content: "Reply with the single word: ok"}},
		MaxTokens: 16,
	})
	return err
}

// CheckTelegram validates a bot token and returns the bot's @username.
func CheckTelegram(ctx context.Context, client *http.Client, apiBase, token string) (string, error) {
	if apiBase == "" {
		apiBase = "https://api.telegram.org"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/bot"+token+"/getMe", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		// Never let the token leak into an error string (it rides in the URL).
		return "", fmt.Errorf("telegram unreachable: %s", strings.ReplaceAll(err.Error(), token, "[token]"))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &payload)
	if !payload.OK {
		if payload.Description != "" {
			return "", fmt.Errorf("telegram rejected the token: %s", payload.Description)
		}
		return "", fmt.Errorf("telegram rejected the token (HTTP %d)", resp.StatusCode)
	}
	return payload.Result.Username, nil
}

// CheckTwilio validates carrier credentials and returns the account's name.
// The auth token rides in an Authorization header, never in the URL, so a
// transport error cannot leak it.
func CheckTwilio(ctx context.Context, client *http.Client, apiBase, accountSID, authToken string) (string, error) {
	if apiBase == "" {
		apiBase = "https://api.twilio.com"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s.json", strings.TrimSuffix(apiBase, "/"), accountSID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(accountSID, authToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("twilio unreachable: %s", redactSecret(err.Error(), authToken))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		FriendlyName string `json:"friendly_name"`
		Status       string `json:"status"`
		Message      string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	if resp.StatusCode != http.StatusOK {
		if payload.Message != "" {
			return "", fmt.Errorf("twilio rejected the credentials: %s", payload.Message)
		}
		return "", fmt.Errorf("twilio rejected the credentials (HTTP %d)", resp.StatusCode)
	}
	if payload.Status != "" && payload.Status != "active" {
		return "", fmt.Errorf("the Twilio account is %s", payload.Status)
	}
	return payload.FriendlyName, nil
}

// CheckElevenLabs validates a text-to-speech key and returns the plan it is on.
func CheckElevenLabs(ctx context.Context, client *http.Client, apiBase, apiKey string) (string, error) {
	if apiBase == "" {
		apiBase = "https://api.elevenlabs.io"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(apiBase, "/")+"/v1/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("xi-api-key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("elevenlabs unreachable: %s", redactSecret(err.Error(), apiKey))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("elevenlabs rejected the key (HTTP %d)", resp.StatusCode)
	}
	var payload struct {
		Subscription struct {
			Tier string `json:"tier"`
		} `json:"subscription"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Subscription.Tier, nil
}

// CheckSpeechServer verifies a local OpenAI-compatible speech server is
// answering, so the local voice tiers fail here rather than mid-call.
func CheckSpeechServer(ctx context.Context, client *http.Client, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/models", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s is not answering: %w", host(baseURL), err)
	}
	defer resp.Body.Close()
	return nil
}

func redactSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// DetectLocalProvider probes the well-known local endpoints (Ollama, LM
// Studio, llama.cpp) so the wizard can offer a free offline fallback.
func DetectLocalProvider(ctx context.Context, client *http.Client) (config.Candidate, bool) {
	type localCandidate struct {
		typ   string
		model string
	}
	for _, lc := range []localCandidate{{"ollama", ""}, {"lmstudio", ""}, {"llamacpp", ""}} {
		cand := config.Candidate{Type: lc.typ}
		fast, cancel := context.WithTimeout(ctx, 2*time.Second)
		models, err := ListModels(fast, client, cand)
		cancel()
		if err != nil || len(models) == 0 {
			continue
		}
		cand.Model = models[0]
		return cand, true
	}
	return config.Candidate{}, false
}

func effectiveBase(cand config.Candidate) string {
	if cand.APIBase != "" {
		return cand.APIBase
	}
	return provider.DefaultAPIBase(cand.Type)
}

func host(base string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexByte(trimmed, '/'); i > 0 {
		return trimmed[:i]
	}
	return trimmed
}

func statusError(code int, body []byte) error {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the API key was rejected (HTTP %d)", code)
	case http.StatusNotFound:
		return fmt.Errorf("this endpoint has no /models listing (HTTP 404)")
	}
	detail := strings.TrimSpace(string(body))
	if len(detail) > 160 {
		detail = detail[:160] + "…"
	}
	if detail == "" {
		return fmt.Errorf("HTTP %d", code)
	}
	return fmt.Errorf("HTTP %d: %s", code, detail)
}

// filterModels narrows a model list by a case-insensitive substring.
func filterModels(models []string, needle string) []string {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return models
	}
	var out []string
	for _, m := range models {
		if strings.Contains(strings.ToLower(m), needle) {
			out = append(out, m)
		}
	}
	return out
}
