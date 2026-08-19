package wizard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyqlelabs/factor/internal/config"
)

// halfBrokenProvider lists models but refuses to complete: exactly what a
// mistyped or withdrawn extraction model looks like from here.
func halfBrokenProvider(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		var data []map[string]string
		for _, m := range models {
			data = append(data, map[string]string{"id": m})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"no such model"}}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// directWiz drives one step on its own. The extraction question depends on
// the provider already chosen, and some of those providers (Anthropic) cannot
// be scripted end to end without reaching their real endpoint.
func directWiz(t *testing.T, cfg *config.Config, answers ...string) (*wiz, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	ui := NewPlain(strings.NewReader(strings.Join(answers, "\n")+"\n"), out)
	opts := Options{
		Home: t.TempDir(),
		HTTP: http.DefaultClient,
		// Never probe the developer's own smrti: a live one on this box would
		// otherwise decide what these tests print.
		MemoryAnswering: func(context.Context, config.MemoryConfig) bool { return false },
	}
	return &wiz{cfg: cfg, ui: ui, opts: opts}, out
}

func sidecarConfig(t *testing.T, baseURL string) *config.Config {
	t.Helper()
	tempHome(t)
	cfg := config.Default()
	cfg.Provider.Type = "custom"
	cfg.Provider.APIBase = baseURL + "/v1"
	cfg.Provider.APIKey = "sk-test"
	cfg.Provider.Model = "small-model"
	cfg.Memory.Mode = "sidecar"
	return cfg
}

func TestWizardPicksAnExtractionModel(t *testing.T) {
	provider := fakeProvider(t, "big-model", "small-model")
	h := newHarness(t,
		"8",                // provider: other OpenAI-compatible
		provider.URL+"/v1", // base URL
		"sk-test",          // api key
		"2",                // model: small-model
		"1",                // reasoning effort: xhigh
		"",                 // do not hide the reasoning text
		"1",                // memory: managed sidecar
		"y",                // install smrti
		"1",                // personality: balanced
		"2",                // extraction model: a cheaper one
		"1",                // big-model, from the endpoint's own list
		"n",                // no telegram
		"n",                // no phone
		"y",                // restrict to workspace
		"n",                // browser off
	)
	if err := h.run(); err != nil {
		t.Fatalf("wizard: %v\n%s", err, h.out.String())
	}
	cfg := h.saved()
	if cfg.Memory.ExtractModel != "big-model" {
		t.Errorf("extract_model = %q, want the model picked from the list", cfg.Memory.ExtractModel)
	}
	if !strings.Contains(h.out.String(), "extract big-model") {
		t.Errorf("the summary does not report the extraction model:\n%s", h.out.String())
	}
}

func TestWizardExtractionModelFollowsTheAgentByDefault(t *testing.T) {
	cfg := sidecarConfig(t, fakeProvider(t, "big-model", "small-model").URL)
	cfg.Memory.ExtractModel = "stale-model"
	w, _ := directWiz(t, cfg, "1") // the one Factor thinks with
	if err := w.askExtractModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ExtractModel != "" {
		t.Errorf("extract_model = %q, want it cleared so the provider model is used", cfg.Memory.ExtractModel)
	}
}

// A model that does not answer is worth saying so about, but the user is
// allowed to keep it: the endpoint may simply be down while they set up.
func TestWizardKeepsAnUnverifiedExtractionModel(t *testing.T) {
	cfg := sidecarConfig(t, halfBrokenProvider(t, "cheap-model", "small-model").URL)
	cfg.Memory.ExtractModel = "cheap-model" // already set: the menu defaults to picking one
	w, out := directWiz(t, cfg,
		"",  // menu default: a cheaper one
		"1", // cheap-model
		"n", // do not pick another after the check fails
	)
	if err := w.askExtractModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ExtractModel != "cheap-model" {
		t.Errorf("extract_model = %q, want the model kept", cfg.Memory.ExtractModel)
	}
	if !strings.Contains(out.String(), "unverified") {
		t.Errorf("no warning about the unverified model:\n%s", out.String())
	}
}

func TestWizardGivesUpOnAnExtractionModelThatNeverAnswers(t *testing.T) {
	cfg := sidecarConfig(t, halfBrokenProvider(t, "cheap-model").URL)
	w, out := directWiz(t, cfg,
		"2",      // a cheaper one
		"1", "y", // pick, fails, try again
		"1", "y", // again
		"1", "y", // and again: out of attempts
	)
	if err := w.askExtractModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ExtractModel != "" {
		t.Errorf("extract_model = %q, want it left to the provider model", cfg.Memory.ExtractModel)
	}
	if !strings.Contains(out.String(), "no model answered") {
		t.Errorf("the failure was not reported:\n%s", out.String())
	}
}

// Anthropic is not OpenAI-compatible, so smrti extracts locally and there is
// no model to choose. Asking anyway would write a setting that does nothing.
func TestWizardSkipsTheExtractionModelWithoutAnOpenAIEndpoint(t *testing.T) {
	tempHome(t)
	cfg := config.Default()
	cfg.Provider.Type = "anthropic"
	cfg.Provider.Model = "claude-sonnet-5"
	cfg.Provider.APIKey = "sk-ant"
	cfg.Memory.Mode = "sidecar"
	w, out := directWiz(t, cfg)
	if err := w.askExtractModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ExtractModel != "" {
		t.Errorf("extract_model = %q, want none", cfg.Memory.ExtractModel)
	}
	if !strings.Contains(out.String(), "runs locally") {
		t.Errorf("the local fallback was not explained:\n%s", out.String())
	}
}

// An external engine reads its own environment; memory.extract_model only
// reaches the sidecar Factor spawns itself.
func TestWizardSkipsTheExtractionModelForAnExternalEngine(t *testing.T) {
	cfg := sidecarConfig(t, fakeProvider(t, "big-model").URL)
	cfg.Memory.Mode = "external"
	w, out := directWiz(t, cfg)
	if err := w.askExtractModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "extract memories") {
		t.Errorf("asked about a setting the external engine never reads:\n%s", out.String())
	}
}

// With Anthropic in front and an OpenAI-compatible fallback behind it, the
// endpoint smrti extracts against is the fallback's: probing it as "anthropic"
// would send an x-api-key header to a server that wants a bearer token.
func TestWizardExtractionEndpointIsTheOpenAICompatibleFallback(t *testing.T) {
	tempHome(t)
	cfg := config.Default()
	cfg.Provider.Type = "anthropic"
	cfg.Provider.Model = "claude-sonnet-5"
	cfg.Provider.APIKey = "sk-ant"
	cfg.Provider.Fallbacks = []config.Candidate{
		{Type: "openrouter", APIKey: "sk-or", Model: "google/gemini-3.6-flash"},
	}
	cand, extract, ok := extractCandidate(cfg)
	if !ok || cand.Type != "custom" || cand.APIKey != "sk-or" {
		t.Fatalf("candidate = %+v, ok = %v", cand, ok)
	}
	if cand.APIBase != "https://openrouter.ai/api/v1" || extract.Model != "google/gemini-3.6-flash" {
		t.Errorf("base = %q, model = %q", cand.APIBase, extract.Model)
	}
}

// An extraction endpoint set by hand has no "model Factor thinks with" behind
// it: smrti picks the default there, and the menu has to say so.
func TestWizardExtractionModelFollowsAnEndpointOfItsOwn(t *testing.T) {
	srv := fakeProvider(t, "big-model")
	cfg := sidecarConfig(t, srv.URL)
	cfg.Memory.ExtractURL = srv.URL + "/v1"
	w, out := directWiz(t, cfg, "1") // whatever the endpoint defaults to
	if err := w.askExtractModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ExtractModel != "" {
		t.Errorf("extract_model = %q, want none", cfg.Memory.ExtractModel)
	}
	if !strings.Contains(out.String(), "Whatever the endpoint defaults to") {
		t.Errorf("the endpoint's own default was not offered:\n%s", out.String())
	}
}

// The environment a running smrti was spawned with is fixed: a model chosen
// here reaches it when Factor starts the next one, and saying so is the
// difference between a setting that looks ignored and one that is pending.
func TestWizardSaysALiveEngineKeepsItsExtractionModel(t *testing.T) {
	cfg := sidecarConfig(t, fakeProvider(t, "big-model", "small-model").URL)
	w, out := directWiz(t, cfg,
		"2", // a cheaper one
		"1", // big-model
	)
	w.opts.MemoryAnswering = func(context.Context, config.MemoryConfig) bool { return true }
	if err := w.askExtractModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.ExtractModel != "big-model" {
		t.Fatalf("extract_model = %q", cfg.Memory.ExtractModel)
	}
	if !strings.Contains(out.String(), "until it is restarted") {
		t.Errorf("the pending restart was not mentioned:\n%s", out.String())
	}
}
