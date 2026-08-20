package wizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
)

func candidate(providerType, key string) config.Candidate {
	return config.Candidate{Type: providerType, APIKey: key, Model: "test-model"}
}

// stubTransport answers every request from a function, which is how the local
// provider probe is driven here: it addresses fixed localhost ports that a
// test machine must not be assumed to have.
type stubTransport func(*http.Request) (*http.Response, error)

func (f stubTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestDetectLocalProviderPicksTheFirstEndpointThatAnswers(t *testing.T) {
	var asked []string
	client := &http.Client{Transport: stubTransport(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.Host)
		if len(asked) == 1 {
			return nil, errors.New("connection refused") // ollama is not running
		}
		return jsonResponse(http.StatusOK, `{"data":[{"id":"qwen3:8b"},{"id":"llama3"}]}`), nil
	})}

	cand, ok := DetectLocalProvider(context.Background(), client)
	if !ok {
		t.Fatal("no local provider detected")
	}
	if cand.Type != "lmstudio" || cand.Model != "llama3" {
		t.Errorf("candidate = %+v; want the second endpoint and its first (sorted) model", cand)
	}
	if len(asked) != 2 {
		t.Errorf("probed %v; should have stopped at the first answer", asked)
	}
}

func TestDetectLocalProviderGivesUpQuietly(t *testing.T) {
	client := &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	if _, ok := DetectLocalProvider(context.Background(), client); ok {
		t.Error("a provider was reported with nothing listening")
	}

	// an endpoint that answers with an empty catalogue is not a usable provider
	client = &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":[]}`), nil
	})}
	if _, ok := DetectLocalProvider(context.Background(), client); ok {
		t.Error("an empty model list was accepted as a provider")
	}
}

func TestListModelsErrorsAreDiagnosable(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, "", "API key was rejected"},
		{"forbidden", http.StatusForbidden, "", "API key was rejected"},
		{"not found", http.StatusNotFound, "", "no /models listing"},
		{"server error with detail", http.StatusInternalServerError, "upstream exploded", "HTTP 500: upstream exploded"},
		{"server error without detail", http.StatusBadGateway, "", "HTTP 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
				return jsonResponse(tc.status, tc.body), nil
			})}
			_, err := ListModels(ctx, client, candidate("openai", "sk-x"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}

	// a long error body is truncated rather than dumped into the terminal
	long := strings.Repeat("x", 500)
	client := &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTeapot, long), nil
	})}
	_, err := ListModels(ctx, client, candidate("openai", "sk-x"))
	if err == nil || len(err.Error()) > 220 || !strings.HasSuffix(err.Error(), "…") {
		t.Errorf("error = %q (len %d); want it truncated", err, len(err.Error()))
	}

	// a 200 that is not a model list is reported as such
	client = &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, "<html>hello</html>"), nil
	})}
	if _, err := ListModels(ctx, client, candidate("openai", "sk-x")); err == nil ||
		!strings.Contains(err.Error(), "unexpected /models response") {
		t.Errorf("error = %v", err)
	}

	// and a provider with no known base URL fails before any request
	if _, err := ListModels(ctx, client, candidate("madeupvendor", "")); err == nil {
		t.Error("a provider with no API base was probed anyway")
	}
}

func TestListModelsSendsProviderSpecificAuth(t *testing.T) {
	var got http.Header
	client := &http.Client{Transport: stubTransport(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Clone()
		return jsonResponse(http.StatusOK, `{"data":[{"id":"claude-x"},{"id":""}]}`), nil
	})}

	models, err := ListModels(context.Background(), client, candidate("anthropic", "sk-ant"))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "claude-x" {
		t.Errorf("models = %v; the empty id should have been dropped", models)
	}
	if got.Get("x-api-key") != "sk-ant" || got.Get("anthropic-version") == "" {
		t.Errorf("anthropic headers = %v", got)
	}

	_, _ = ListModels(context.Background(), client, candidate("openai", "sk-oai"))
	if got.Get("Authorization") != "Bearer sk-oai" {
		t.Errorf("openai headers = %v", got)
	}
}

// The bot token rides in the URL, so it must never reach an error string.
func TestCheckTelegramNeverLeaksTheToken(t *testing.T) {
	const token = "12345:SUPER-SECRET"
	client := &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup api.telegram.org/bot" + token + "/getMe")
	})}
	_, err := CheckTelegram(context.Background(), client, "", token)
	if err == nil {
		t.Fatal("an unreachable API reported success")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "[token]") {
		t.Errorf("error = %v", err)
	}
}

func TestCheckTelegramRejections(t *testing.T) {
	ctx := context.Background()

	client := &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"ok":false,"description":"Unauthorized"}`), nil
	})}
	if _, err := CheckTelegram(ctx, client, "", "t"); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error = %v", err)
	}

	// a rejection with no description still says what happened
	client = &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusInternalServerError, `nonsense`), nil
	})}
	if _, err := CheckTelegram(ctx, client, "", "t"); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v", err)
	}

	client = &http.Client{Transport: stubTransport(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"result":{"username":"factorbot"}}`), nil
	})}
	if name, err := CheckTelegram(ctx, client, "https://tg.example", "t"); err != nil || name != "factorbot" {
		t.Errorf("username = %q, %v", name, err)
	}
}

func TestHostStripsSchemeAndPath(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1": "api.openai.com",
		"http://127.0.0.1:11434/v1": "127.0.0.1:11434",
		"api.example.com":           "api.example.com",
	}
	for base, want := range cases {
		if got := host(base); got != want {
			t.Errorf("host(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestFilterModels(t *testing.T) {
	models := []string{"gpt-5", "claude-opus", "GPT-4o"}
	if got := filterModels(models, "  "); len(got) != 3 {
		t.Errorf("a blank needle filtered anything: %v", got)
	}
	got := filterModels(models, "gpt")
	if len(got) != 2 || got[0] != "gpt-5" || got[1] != "GPT-4o" {
		t.Errorf("filtered = %v; the match should be case-insensitive", got)
	}
	if got := filterModels(models, "nothing"); got != nil {
		t.Errorf("filtered = %v, want nothing", got)
	}
}

// A model name is only carried across a provider switch when it plausibly
// belongs to the new provider.
func TestModelLooksLike(t *testing.T) {
	cases := []struct {
		model, provider string
		want            bool
	}{
		{"", "openai", false},
		{"anthropic/claude-opus", "openrouter", true},
		{"gpt-5", "openrouter", false},
		{"claude-opus-5", "anthropic", true},
		{"gpt-5", "anthropic", false},
		{"gpt-5", "openai", true},
		{"o3", "openai", true},
		{"claude-opus-5", "openai", false},
		{"qwen3:8b", "ollama", true},
	}
	for _, tc := range cases {
		if got := modelLooksLike(tc.model, tc.provider); got != tc.want {
			t.Errorf("modelLooksLike(%q, %q) = %v, want %v", tc.model, tc.provider, got, tc.want)
		}
	}
}

func TestSmallHelpers(t *testing.T) {
	if got := firstNonEmpty("", "", "third", "fourth"); got != "third" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	if got := firstLine("one\ntwo"); got != "one" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstPositive(0, 0, 7, 9); got != 7 {
		t.Errorf("firstPositive = %d", got)
	}
	if got := firstPositive(0, 0); got != 0 {
		t.Errorf("firstPositive = %d", got)
	}

	dir := t.TempDir()
	t.Setenv("PATH", strings.Join([]string{"/usr/bin", dir}, string(os.PathListSeparator)))
	if !onPath(dir) {
		t.Errorf("onPath(%q) = false", dir)
	}
	if onPath(filepath.Join(dir, "nope")) {
		t.Error("onPath accepted a directory that is not on PATH")
	}
}

// A carrier credential must never reach the screen. Transport errors quote
// the request, so a probe that fails against a URL carrying the token would
// print it — which is why every carrier probe redacts before it reports.
func TestCarrierProbesKeepCredentialsOutOfTheirErrors(t *testing.T) {
	const token = "super-secret-auth-token"
	// A transport that fails with the credential in the message is the worst
	// case these call sites exist to contain.
	client := &http.Client{Transport: stubTransport(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial tcp: proxy rejected %s?auth=%s", r.URL.Host, token)
	})}
	ctx := context.Background()

	probes := map[string]func() error{
		"twilio": func() error {
			_, err := CheckTwilio(ctx, client, "https://api.twilio.test", "AC123", token)
			return err
		},
		"telnyx": func() error {
			_, err := CheckTelnyx(ctx, client, "https://api.telnyx.test", token, "conn-1")
			return err
		},
		"elevenlabs": func() error {
			_, err := CheckElevenLabs(ctx, client, "https://api.elevenlabs.test", token)
			return err
		},
	}
	for name, probe := range probes {
		err := probe()
		if err == nil {
			t.Errorf("%s: an unreachable carrier reported success", name)
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("%s leaked the credential: %s", name, err)
		}
		if !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("%s error does not show the redaction: %s", name, err)
		}
	}
}

func TestRedactSecretLeavesTextAloneWithoutASecret(t *testing.T) {
	msg := "dial tcp 127.0.0.1:443: connection refused"
	if got := redactSecret(msg, ""); got != msg {
		t.Errorf("an empty secret rewrote the message: %q", got)
	}
}
