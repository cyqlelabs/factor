package wizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/browser"
	"github.com/cyqlelabs/factor/internal/channel/phone"
	"github.com/cyqlelabs/factor/internal/channel/voice"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/tools"
)

// Options configures a wizard run. The function fields exist so tests can
// drive the whole flow without touching the network, the package manager, or
// the user's Python installation.
type Options struct {
	UI         *UI
	Version    string
	HTTP       *http.Client
	Home       string // FACTOR_HOME (defaults to config.Home())
	Telegram   string // Telegram API base (defaults to the real one)
	Twilio     string // Twilio API base (defaults to the real one)
	Telnyx     string // Telnyx API base (defaults to the real one)
	ElevenLabs string // ElevenLabs API base (defaults to the real one)

	// NonInteractive skips every prompt: defaults are kept, smrti is
	// installed when missing, and the config is written as-is.
	NonInteractive bool
	// NoInstall suppresses installing smrti and desktop helpers.
	NoInstall bool

	EnsureSmrti func(ctx context.Context, cfg config.MemoryConfig, progress memory.Progress) (path string, installed bool, err error)
	// MemoryAnswering reports whether a smrti is already serving. Someone
	// running one in Docker or on another box has a working memory with no
	// local binary to find, and must not be offered an install over it.
	MemoryAnswering func(ctx context.Context, cfg config.MemoryConfig) bool
	InstallPackages func(ctx context.Context, packages []string) (string, error)
	Desktop         desktop.Env

	// Audio is the seam to the machine's sound system, for the PC voice
	// step: probing for a sound card and for the capture/playback helpers.
	Audio voice.Env

	// InstallSpeech puts the local speech engines and their models on the
	// machine. Choosing a local tier is a request for local speech, not for
	// homework, so the wizard does this rather than telling the user to.
	InstallSpeech func(ctx context.Context, language string, needSTT, needTTS bool,
		progress phone.Progress) (phone.SpeechChoices, error)

	// EnsureBrowser puts a browser on the machine, and VerifyBrowser proves
	// the configured one really drives. Same reasoning as speech: enabling
	// the browser tools is a request to browse, not a request for homework.
	EnsureBrowser func(ctx context.Context, progress browser.Progress) (path string, installed bool, err error)
	VerifyBrowser func(ctx context.Context, cfg config.BrowserConfig) error

	// EnsureFastBrowser installs the optional read-only engine, and
	// FastBrowserSupported reports whether this machine could run it at all.
	EnsureFastBrowser    func(ctx context.Context, progress browser.Progress) (path string, installed bool, err error)
	FastBrowserSupported func() (bool, string)
}

func (o *Options) defaults() {
	if o.UI == nil {
		o.UI = New(os.Stdin, os.Stdout)
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: probeTimeout}
	}
	if o.Home == "" {
		o.Home = config.Home()
	}
	if o.Desktop.Run == nil {
		o.Desktop = desktop.DefaultEnv()
	}
	if o.Audio.Has == nil {
		o.Audio = voice.DefaultEnv()
	}
	if o.MemoryAnswering == nil {
		o.MemoryAnswering = memory.Answering
	}
	if o.EnsureSmrti == nil {
		o.EnsureSmrti = func(ctx context.Context, cfg config.MemoryConfig, progress memory.Progress) (string, bool, error) {
			return memory.EnsureSmrti(ctx, cfg.Command, o.Home, true, progress)
		}
	}
	if o.InstallSpeech == nil {
		o.InstallSpeech = func(ctx context.Context, language string, needSTT, needTTS bool,
			progress phone.Progress) (phone.SpeechChoices, error) {
			return phone.InstallSpeech(ctx, o.Home, language, phone.SpeechConfig{}, needSTT, needTTS, progress)
		}
	}
	if o.EnsureBrowser == nil {
		o.EnsureBrowser = func(ctx context.Context, progress browser.Progress) (string, bool, error) {
			return browser.EnsureEngine(ctx, o.Home, progress)
		}
	}
	if o.VerifyBrowser == nil {
		o.VerifyBrowser = browser.Verify
	}
	if o.FastBrowserSupported == nil {
		o.FastBrowserSupported = browser.FastEngineSupported
	}
	if o.EnsureFastBrowser == nil {
		o.EnsureFastBrowser = func(ctx context.Context, progress browser.Progress) (string, bool, error) {
			return browser.EnsureFastEngine(ctx, o.Home, progress)
		}
	}
	if o.InstallPackages == nil {
		o.InstallPackages = func(ctx context.Context, packages []string) (string, error) {
			args := make([]any, 0, len(packages))
			for _, p := range packages {
				args = append(args, p)
			}
			res := tools.NewPkgInstallTool().Execute(ctx, map[string]any{"packages": args, "manager": "auto"})
			if res.IsError {
				return res.ForLLM, errors.New(firstLine(res.ForLLM))
			}
			return res.ForLLM, nil
		}
	}
}

// geteuid is a seam: the root check decides a config value, so tests need to
// drive both sides of it without running as root.
var geteuid = os.Geteuid

const totalSteps = 5

type wiz struct {
	cfg  *config.Config
	ui   *UI
	opts Options
}

// Run walks the user through setup and writes the config file at path
// ("" = the default location). It edits the on-disk config, never the
// env-overlaid one, so secrets exported in the environment stay out of the
// file.
func Run(ctx context.Context, path string, opts Options) error {
	opts.defaults()
	cfg, err := config.ReadFile(path)
	if err != nil {
		return err
	}
	w := &wiz{cfg: cfg, ui: opts.UI, opts: opts}

	if opts.NonInteractive || !opts.UI.Interactive() {
		return w.runQuiet(ctx)
	}

	w.ui.Banner(opts.Version)
	if _, err := os.Stat(cfg.Path()); err == nil {
		w.ui.Note("editing the existing config at %s", cfg.Path())
	} else {
		w.ui.Note("this will create %s", cfg.Path())
	}

	for _, step := range []func(context.Context) error{
		w.stepProvider,
		w.stepMemory,
		w.stepChannels,
		w.stepDesktop,
		w.stepFinish,
	} {
		if err := step(ctx); err != nil {
			if errors.Is(err, ErrAborted) {
				w.ui.printf("\n")
				w.ui.Warn("setup cancelled — nothing was written")
			}
			return err
		}
	}
	return nil
}

// runQuiet is the scriptable path: create everything, install smrti when it
// is missing, report, and never prompt.
func (w *wiz) runQuiet(ctx context.Context) error {
	if err := config.EnsureWorkspace(w.cfg.Agent.Workspace); err != nil {
		return err
	}
	if err := w.cfg.Save(); err != nil {
		return err
	}
	w.ui.printf("config:    %s\n", w.cfg.Path())
	w.ui.printf("workspace: %s\n", w.cfg.Agent.Workspace)

	// Only a sidecar Factor supervises itself needs a local binary: an
	// external engine is somebody else's process, and installing a second
	// copy beside it helps nobody.
	if w.cfg.Memory.Mode == "sidecar" && !w.opts.NoInstall && !w.opts.MemoryAnswering(ctx, w.cfg.Memory) {
		path, installed, err := w.opts.EnsureSmrti(ctx, w.cfg.Memory, func(format string, args ...any) {
			w.ui.printf("smrti:     %s\n", fmt.Sprintf(format, args...))
		})
		switch {
		case err != nil:
			w.ui.printf("smrti:     NOT installed — %v\n", err)
		case installed:
			w.ui.printf("smrti:     installed at %s\n", path)
		default:
			w.ui.printf("smrti:     found at %s\n", path)
		}
	}
	if !w.opts.NoInstall {
		w.quietDesktopHelpers(ctx)
		w.quietAudioHelpers(ctx)
	}
	if w.cfg.Browser.Enabled && !w.opts.NoInstall && browser.Available() {
		if err := w.quietBrowser(ctx); err != nil {
			return err
		}
	}
	if w.cfg.Provider.APIKey == "" && os.Getenv("FACTOR_PROVIDER_API_KEY") == "" {
		w.ui.printf("provider:  no API key — export FACTOR_PROVIDER_API_KEY or run `factor init` interactively\n")
	}
	return nil
}

// quietDesktopHelpers installs the desktop helpers without asking. Setup is
// where dependencies get met, whether or not a human is watching it happen.
func (w *wiz) quietDesktopHelpers(ctx context.Context) {
	env := w.opts.Desktop
	if !desktop.MachineHasDisplay(env) {
		return
	}
	missing := desktop.MissingHelpers(env, desktop.NewController(env))
	if len(missing) == 0 {
		return
	}
	manager := tools.DetectSystemManager()
	if manager == "" {
		w.ui.printf("desktop:   missing %s and no package manager to install them\n", helperNames(missing))
		return
	}
	packages := desktop.PackagesFor(missing, manager)
	if _, err := w.opts.InstallPackages(ctx, packages); err != nil {
		w.ui.printf("desktop:   %s NOT installed — %v\n", strings.Join(packages, " "), err)
		return
	}
	w.ui.printf("desktop:   installed %s\n", strings.Join(packages, " "))
}

// quietAudioHelpers meets the PC voice channel's dependencies without asking,
// but only when the channel is configured: a machine that never listens has
// no use for capture helpers.
func (w *wiz) quietAudioHelpers(ctx context.Context) {
	if _, ok := w.cfg.Channels["voice"]; !ok {
		return
	}
	env := w.opts.Audio
	if !voice.MachineHasAudio(env) {
		return
	}
	missing := voice.MissingHelpers(env)
	if len(missing) == 0 {
		return
	}
	manager := tools.DetectSystemManager()
	if manager == "" {
		w.ui.printf("voice:     missing %s and no package manager to install them\n", helperNames(missing))
		return
	}
	packages := desktop.PackagesFor(missing, manager)
	if _, err := w.opts.InstallPackages(ctx, packages); err != nil {
		w.ui.printf("voice:     %s NOT installed — %v\n", strings.Join(packages, " "), err)
		return
	}
	w.ui.printf("voice:     installed %s\n", strings.Join(packages, " "))
}

func helperNames(helpers []desktop.Helper) string {
	names := make([]string, 0, len(helpers))
	for _, h := range helpers {
		names = append(names, h.Bin)
	}
	return strings.Join(names, ", ")
}

// quietBrowser gives the scriptable path the same browser the interactive one
// gets: an unattended install is exactly where nobody is watching to fix it.
func (w *wiz) quietBrowser(ctx context.Context) error {
	if geteuid() == 0 {
		w.cfg.Browser.NoSandbox = true
	}
	// The configured browser comes first: a machine that already has one
	// must not be made to download another.
	path, err := browser.FindBrowserBinary(w.cfg.Browser.Command)
	if err != nil {
		var installErr error
		path, _, installErr = w.opts.EnsureBrowser(ctx, func(format string, args ...any) {
			w.ui.printf("browser:   %s\n", fmt.Sprintf(format, args...))
		})
		if installErr != nil {
			w.ui.printf("browser:   NOT installed — %v\n", installErr)
			return nil
		}
	}
	w.cfg.Browser.Command = path
	w.ui.printf("browser:   %s\n", path)
	return w.cfg.Save()
}

// ---- step 1: provider ------------------------------------------------------

type providerPreset struct {
	Type     string
	Label    string
	Hint     string
	Model    string
	NeedsKey bool
	KeyURL   string
	Custom   bool
}

var providerPresets = []providerPreset{
	{Type: "openrouter", Label: "OpenRouter", Hint: "one key, every major model", Model: "google/gemini-3.1-pro-preview", NeedsKey: true, KeyURL: "https://openrouter.ai/keys"},
	{Type: "anthropic", Label: "Anthropic", Hint: "Claude, native API", Model: "claude-sonnet-5", NeedsKey: true, KeyURL: "https://console.anthropic.com/settings/keys"},
	{Type: "openai", Label: "OpenAI", Hint: "GPT models", Model: "gpt-5", NeedsKey: true, KeyURL: "https://platform.openai.com/api-keys"},
	{Type: "groq", Label: "Groq", Hint: "very fast open models", Model: "llama-3.3-70b-versatile", NeedsKey: true, KeyURL: "https://console.groq.com/keys"},
	{Type: "ollama", Label: "Ollama", Hint: "local, free, no key (127.0.0.1:11434)", Model: "qwen3:8b"},
	{Type: "lmstudio", Label: "LM Studio", Hint: "local (127.0.0.1:1234)"},
	{Type: "llamacpp", Label: "llama.cpp", Hint: "local (127.0.0.1:8080)"},
	{Type: "custom", Label: "Other OpenAI-compatible", Hint: "enter your own base URL", NeedsKey: true, Custom: true},
}

func (w *wiz) stepProvider(ctx context.Context) error {
	w.ui.Step(1, totalSteps, "Language model provider")

	opts := make([]Option, len(providerPresets))
	def := 0
	for i, p := range providerPresets {
		opts[i] = Option{Label: p.Label, Hint: p.Hint}
		if p.Type == w.cfg.Provider.Type {
			def = i
		}
	}
	idx, err := w.ui.Select("Which provider should Factor talk to?", opts, def)
	if err != nil {
		return err
	}
	preset := providerPresets[idx]

	previousType := w.cfg.Provider.Type
	w.cfg.Provider.Type = preset.Type
	w.cfg.Provider.APIBase = ""
	if preset.Custom {
		base, err := w.ui.Input("Base URL (OpenAI-compatible, including /v1)", w.cfg.Provider.APIBase)
		if err != nil {
			return err
		}
		if strings.TrimSpace(base) == "" {
			return fmt.Errorf("a custom provider needs a base URL")
		}
		w.cfg.Provider.APIBase = strings.TrimSpace(base)
	}

	if preset.NeedsKey {
		if envKey := os.Getenv("FACTOR_PROVIDER_API_KEY"); envKey != "" {
			w.ui.Note("FACTOR_PROVIDER_API_KEY is set in your environment; it overrides the config file")
		}
		if preset.KeyURL != "" {
			w.ui.Note("get a key at %s", preset.KeyURL)
		}
		key, err := w.ui.Secret("API key", w.cfg.Provider.APIKey)
		if err != nil {
			return err
		}
		w.cfg.Provider.APIKey = strings.TrimSpace(key)
		if w.cfg.Provider.APIKey == "" && os.Getenv("FACTOR_PROVIDER_API_KEY") == "" {
			w.ui.Warn("no API key — Factor will not be able to think until you add one")
		}
	} else {
		w.cfg.Provider.APIKey = ""
	}

	cand := config.Candidate{Type: w.cfg.Provider.Type, APIKey: w.cfg.Provider.APIKey, APIBase: w.cfg.Provider.APIBase}
	// Keep the configured model only if it still fits the chosen provider;
	// switching OpenRouter → Ollama must not carry "anthropic/claude-…" over.
	defaultModel := w.cfg.Provider.Model
	if preset.Model != "" && (defaultModel == "" || previousType != preset.Type || !modelLooksLike(defaultModel, preset.Type)) {
		defaultModel = preset.Model
	}

	// With no credentials there is nothing to probe: asking the network would
	// only stall the wizard on a guaranteed 401.
	if preset.NeedsKey && cand.APIKey == "" {
		model, err := w.ui.Input("Model", defaultModel)
		if err != nil {
			return err
		}
		w.cfg.Provider.Model = model
		w.ui.Note("add the key later with: factor config set provider.api_key <key>")
		return w.askReasoning(preset.Type)
	}

	var models []string
	_ = w.ui.Task("fetching the model list", func() error {
		var err error
		models, err = ListModels(ctx, w.opts.HTTP, cand)
		return err
	})
	model, err := w.chooseModel(models, defaultModel)
	if err != nil {
		return err
	}
	w.cfg.Provider.Model = model
	cand.Model = model

	if err := w.askReasoning(preset.Type); err != nil {
		return err
	}
	cand.Reasoning = &w.cfg.Provider.Reasoning
	return w.verifyProvider(ctx, cand)
}

// reasoningLevels are the efforts Factor can ask for. The wire spelling
// differs per backend (OpenRouter reasoning.effort, OpenAI reasoning_effort,
// Anthropic thinking budgets) — the provider layer translates.
var reasoningLevels = []Option{
	{Label: "xhigh", Hint: "think as hard as the model allows (default)"},
	{Label: "high", Hint: "deep reasoning"},
	{Label: "medium", Hint: "balanced"},
	{Label: "low", Hint: "quick"},
	{Label: "none", Hint: "no reasoning parameters at all"},
	{Label: "custom token budget", Hint: "set an exact thinking budget"},
}

// askReasoning offers the reasoning controls the chosen provider supports.
// Not every backend has them: local servers reject the parameters outright,
// so the question is skipped rather than asked and ignored.
func (w *wiz) askReasoning(providerType string) error {
	if !provider.SupportsReasoning(providerType) {
		w.cfg.Provider.Reasoning = config.ReasoningConfig{}
		w.ui.Note("reasoning controls are not available for local servers — skipping")
		return nil
	}

	current := w.cfg.Provider.Reasoning
	def := 0
	switch {
	case current.MaxTokens > 0:
		def = len(reasoningLevels) - 1
	case current.Effort != "":
		for i, level := range reasoningLevels {
			if level.Label == current.Effort {
				def = i
			}
		}
	}
	idx, err := w.ui.Select("Reasoning effort", reasoningLevels, def)
	if err != nil {
		return err
	}

	next := config.ReasoningConfig{}
	if idx == len(reasoningLevels)-1 {
		answer, err := w.ui.Input("Thinking budget in tokens", itoa(firstPositive(current.MaxTokens, 32768)))
		if err != nil {
			return err
		}
		budget, convErr := strconv.Atoi(strings.TrimSpace(answer))
		if convErr != nil || budget <= 0 {
			w.ui.Warn("%q is not a token count — falling back to effort=high", answer)
			next.Effort = "high"
		} else {
			next.MaxTokens = budget
		}
	} else {
		next.Effort = reasoningLevels[idx].Label
	}

	if next.Effort != "none" {
		exclude, err := w.ui.Confirm("Keep the reasoning text out of replies (it is still billed)?", current.Exclude)
		if err != nil {
			return err
		}
		next.Exclude = exclude
	}
	w.cfg.Provider.Reasoning = next
	if providerType == "anthropic" && next.Effort != "" && next.Effort != "none" {
		w.ui.Note("Anthropic takes a thinking budget: %s becomes %d tokens", next.Effort, provider.EffortBudget(next.Effort))
	}
	return nil
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func itoa(n int) string { return strconv.Itoa(n) }

// modelLooksLike keeps a previously configured model only when it plausibly
// belongs to the newly chosen provider (switching OpenRouter → Ollama should
// not keep "anthropic/claude-…").
func modelLooksLike(model, providerType string) bool {
	if model == "" {
		return false
	}
	switch providerType {
	case "openrouter":
		return strings.Contains(model, "/")
	case "anthropic":
		return strings.HasPrefix(model, "claude")
	case "openai":
		return strings.HasPrefix(model, "gpt") || strings.HasPrefix(model, "o")
	}
	return true
}

const maxModelMenu = 12

func (w *wiz) chooseModel(models []string, def string) (string, error) {
	if len(models) == 0 {
		return w.ui.Input("Model", def)
	}
	for {
		list := models
		if len(list) > maxModelMenu {
			needle, err := w.ui.Input(
				fmt.Sprintf("%d models available — type a filter (e.g. \"sonnet\", blank to browse)", len(models)), "")
			if err != nil {
				return "", err
			}
			list = filterModels(models, needle)
			if len(list) == 0 {
				w.ui.Note("nothing matches %q", needle)
				continue
			}
		}
		truncated := false
		if len(list) > maxModelMenu {
			list, truncated = list[:maxModelMenu], true
		}
		opts := make([]Option, 0, len(list)+2)
		selected := 0
		for i, m := range list {
			if m == def {
				selected = i
			}
			opts = append(opts, Option{Label: m})
		}
		if truncated {
			opts = append(opts, Option{Label: "↻ filter again", Hint: fmt.Sprintf("%d more not shown", len(models)-maxModelMenu)})
		}
		opts = append(opts, Option{Label: "✎ type a model name", Hint: def})

		idx, err := w.ui.Select("Model", opts, selected)
		if err != nil {
			return "", err
		}
		switch {
		case idx < len(list):
			return list[idx], nil
		case truncated && idx == len(list):
			continue
		default:
			return w.ui.Input("Model", def)
		}
	}
}

// maxVerifyAttempts stops a wizard driven by a script (or an impatient
// Enter-holder) from looping forever against a broken endpoint.
const maxVerifyAttempts = 3

func (w *wiz) verifyProvider(ctx context.Context, cand config.Candidate) error {
	for attempt := 0; attempt < maxVerifyAttempts; attempt++ {
		err := w.ui.Task(fmt.Sprintf("checking %s / %s", cand.Type, cand.Model), func() error {
			return CheckProvider(ctx, cand)
		})
		if err == nil {
			return nil
		}
		idx, selErr := w.ui.Select("That did not work. What now?", []Option{
			{Label: "Re-enter the API key"},
			{Label: "Pick a different model"},
			{Label: "Continue anyway", Hint: "fix it later in the config"},
		}, 2)
		if selErr != nil {
			return selErr
		}
		switch idx {
		case 0:
			key, err := w.ui.Secret("API key", "")
			if err != nil {
				return err
			}
			w.cfg.Provider.APIKey = strings.TrimSpace(key)
			cand.APIKey = w.cfg.Provider.APIKey
		case 1:
			var models []string
			_ = w.ui.Task("fetching the model list", func() error {
				var err error
				models, err = ListModels(ctx, w.opts.HTTP, cand)
				return err
			})
			model, err := w.chooseModel(models, cand.Model)
			if err != nil {
				return err
			}
			w.cfg.Provider.Model = model
			cand.Model = model
		default:
			return nil
		}
	}
	w.ui.Warn("continuing with an unverified provider — check it with `factor status`")
	return nil
}

// ---- step 2: memory --------------------------------------------------------

var personalities = []Option{
	{Label: "balanced", Hint: "steady recall, sensible forgetting"},
	{Label: "analytical", Hint: "facts and structure first"},
	{Label: "curious", Hint: "explores connections"},
	{Label: "empathetic", Hint: "weights emotional context"},
	{Label: "maverick", Hint: "keeps the odd, surprising memories"},
	{Label: "deterministic", Hint: "reproducible, minimal drift"},
}

func (w *wiz) stepMemory(ctx context.Context) error {
	w.ui.Step(2, totalSteps, "Long-term memory (smrti)")
	w.ui.Note("smrti is Factor's memory: episodes, consolidation, and hard lessons from past failures")

	modes := []Option{
		{Label: "Managed sidecar", Hint: "Factor starts and supervises smrti locally (recommended)"},
		{Label: "External server", Hint: "connect to a smrti you already run"},
		{Label: "No long-term memory", Hint: "session history only"},
	}
	def := 0
	switch w.cfg.Memory.Mode {
	case "external":
		def = 1
	case "off":
		def = 2
	}
	idx, err := w.ui.Select("How should memory work?", modes, def)
	if err != nil {
		return err
	}
	switch idx {
	case 1:
		w.cfg.Memory.Mode = "external"
		url, err := w.ui.Input("smrti URL", firstNonEmpty(w.cfg.Memory.URL, "http://127.0.0.1:8420"))
		if err != nil {
			return err
		}
		w.cfg.Memory.URL = url
	case 2:
		w.cfg.Memory.Mode = "off"
		w.ui.Note("you can switch it back on later with: factor config set memory.mode sidecar")
		return nil
	default:
		w.cfg.Memory.Mode = "sidecar"
		w.cfg.Memory.URL = ""
		if err := w.ensureSmrti(ctx); err != nil {
			return err
		}
	}

	pIdx := 0
	for i, p := range personalities {
		if p.Label == w.cfg.Memory.Personality {
			pIdx = i
		}
	}
	pIdx, err = w.ui.Select("Memory personality", personalities, pIdx)
	if err != nil {
		return err
	}
	w.cfg.Memory.Personality = personalities[pIdx].Label
	return nil
}

func (w *wiz) ensureSmrti(ctx context.Context) error {
	if w.opts.MemoryAnswering(ctx, w.cfg.Memory) {
		w.ui.Success("smrti is already answering at %s", w.cfg.Memory.BaseURL())
		return nil
	}
	if path, ok := memory.FindSmrti(w.cfg.Memory.Command, w.opts.Home); ok {
		w.ui.Success("smrti found at %s", path)
		return nil
	}
	if w.opts.NoInstall {
		w.ui.Warn("smrti is not installed (install it with: pip install smrti)")
		return nil
	}
	install, err := w.ui.Confirm("smrti is not installed. Install it now?", true)
	if err != nil {
		return err
	}
	if !install {
		w.cfg.Memory.AutoInstall = true
		w.ui.Note("Factor will try again on first start (memory.auto_install)")
		return nil
	}
	progress := w.ui.Progress()
	var path string
	installErr := w.ui.Task("installing smrti", func() error {
		p, _, err := w.opts.EnsureSmrti(ctx, w.cfg.Memory, progress)
		path = p
		return err
	})
	if installErr != nil {
		w.ui.Note("install it manually with `pip install smrti`, or `uv tool install smrti`")
		w.cfg.Memory.AutoInstall = true
		return nil
	}
	w.ui.Success("smrti installed at %s", path)
	if dir := filepath.Dir(path); !onPath(dir) {
		w.ui.Note("%s is not on your PATH; Factor will use the full path", dir)
	}
	return nil
}

// ---- step 3: channels ------------------------------------------------------

func (w *wiz) stepChannels(ctx context.Context) error {
	w.ui.Step(3, totalSteps, "Channels")
	w.ui.Note("the CLI always works; add Telegram to talk to Factor from your phone")

	if err := w.stepTelegram(ctx); err != nil {
		return err
	}
	if err := w.stepPhone(ctx); err != nil {
		return err
	}
	return w.stepVoice(ctx)
}

func (w *wiz) stepTelegram(ctx context.Context) error {
	existing := telegramConfig(w.cfg)
	want, err := w.ui.Confirm("Set up Telegram now?", existing.Token != "")
	if err != nil {
		return err
	}
	if !want {
		return w.askKeepChannel("telegram", "Telegram")
	}

	w.ui.Note("create a bot with @BotFather, then paste the token it gives you")
	token, err := w.ui.Secret("Bot token", existing.Token)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		w.ui.Warn("no token — skipping Telegram")
		return nil
	}

	var username string
	_ = w.ui.Task("verifying the bot token", func() error {
		var err error
		username, err = CheckTelegram(ctx, w.opts.HTTP, w.opts.Telegram, token)
		return err
	})
	if username != "" {
		w.ui.Success("connected to @%s", username)
	}

	w.ui.Note("send /start to your bot, or ask @userinfobot for your numeric id")
	allow, err := w.ui.Input("Allowed sender ids (comma-separated, blank = anyone)",
		strings.Join(existing.AllowFrom, ","))
	if err != nil {
		return err
	}
	var allowFrom []string
	for _, part := range strings.Split(allow, ",") {
		if p := strings.TrimSpace(part); p != "" {
			allowFrom = append(allowFrom, p)
		}
	}
	if len(allowFrom) == 0 {
		w.ui.Warn("anyone who finds the bot will be able to talk to Factor")
	}

	raw, err := json.Marshal(map[string]any{"token": token, "allow_from": allowFrom})
	if err != nil {
		return err
	}
	if w.cfg.Channels == nil {
		w.cfg.Channels = map[string]json.RawMessage{}
	}
	w.cfg.Channels["telegram"] = raw
	w.ui.Note("run `factor gateway` to bring the bot online")
	return nil
}

type telegramSection struct {
	Token     string   `json:"token"`
	AllowFrom []string `json:"allow_from"`
}

// askKeepChannel is what declining a channel's setup means for a section that
// already exists: the question the wizard used to skip. "No" to "set up X?"
// reads as "X off" — and a section left in the config is an enabled channel,
// so a user who declined Telegram still had a bot polling. The answer is
// written onto the raw section, so fields the wizard's mirrors don't know
// about survive.
func (w *wiz) askKeepChannel(name, label string) error {
	raw, ok := w.cfg.Channels[name]
	if !ok {
		return nil
	}
	enabled := sectionEnabled(raw)
	keep, err := w.ui.Confirm(fmt.Sprintf("%s is already configured. Keep it enabled?", label), enabled)
	if err != nil {
		return err
	}
	if err := w.setChannelEnabled(name, keep); err != nil {
		return err
	}
	if !keep {
		w.ui.Note("disabled — the settings stay in the config; re-run `factor init` to switch it back on")
	}
	return nil
}

// sectionEnabled mirrors the connector rule: absent means on.
func sectionEnabled(raw json.RawMessage) bool {
	var section struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &section); err != nil {
		return true
	}
	return section.Enabled == nil || *section.Enabled
}

// setChannelEnabled flips one channel section's enabled flag in place,
// leaving every other field exactly as it was.
func (w *wiz) setChannelEnabled(name string, enabled bool) error {
	var section map[string]any
	if err := json.Unmarshal(w.cfg.Channels[name], &section); err != nil {
		return fmt.Errorf("channels.%s: %w", name, err)
	}
	if enabled {
		delete(section, "enabled")
	} else {
		section["enabled"] = false
	}
	raw, err := json.Marshal(section)
	if err != nil {
		return err
	}
	w.cfg.Channels[name] = raw
	return nil
}

func telegramConfig(cfg *config.Config) telegramSection {
	var section telegramSection
	if raw, ok := cfg.Channels["telegram"]; ok {
		_ = json.Unmarshal(raw, &section)
	}
	return section
}

// ---- step 3b: the phone ----------------------------------------------------

type audioSection struct {
	Provider string `json:"provider,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
	Voice    string `json:"voice,omitempty"`
}

type phoneSection struct {
	UserNumber       string `json:"user_number"`
	PhoneNumber      string `json:"phone_number"`
	Carrier          string `json:"carrier,omitempty"`
	TwilioAccountSID string `json:"twilio_account_sid,omitempty"`
	TwilioAuthToken  string `json:"twilio_auth_token,omitempty"`

	TelnyxAPIKey       string `json:"telnyx_api_key,omitempty"`
	TelnyxConnectionID string `json:"telnyx_connection_id,omitempty"`
	TelnyxPublicKey    string `json:"telnyx_public_key,omitempty"`

	ElevenLabsAPIKey string       `json:"elevenlabs_api_key,omitempty"`
	VoiceID          string       `json:"voice_id,omitempty"`
	Language         string       `json:"language,omitempty"`
	STT              audioSection `json:"stt,omitempty"`
	STTAPIKey        string       `json:"stt_api_key,omitempty"`
	TTS              audioSection `json:"tts,omitempty"`
	Proactive        string       `json:"proactive,omitempty"`

	// SpeechServer records what the installer chose for this machine and
	// language. It is a pointer so a cloud tier writes no section at all.
	SpeechServer *speechSection `json:"speech_server,omitempty"`
}

// speechSection mirrors channels.phone.speech_server.
type speechSection struct {
	WhisperModel   string `json:"whisper_model,omitempty"`
	WhisperDevice  string `json:"whisper_device,omitempty"`
	WhisperCompute string `json:"whisper_compute,omitempty"`
	PiperVoice     string `json:"piper_voice,omitempty"`
}

func phoneConfig(cfg *config.Config) phoneSection {
	var section phoneSection
	if raw, ok := cfg.Channels["phone"]; ok {
		_ = json.Unmarshal(raw, &section)
	}
	return section
}

// The speech tier is the one voice decision with real trade-offs, so it is
// asked plainly: cloud is the reliable, low-latency default that keeps this
// machine free, and the local tiers trade RAM for privacy and per-minute cost.
var voiceTiers = []Option{
	{Label: "Cloud speech (recommended)", Hint: "lowest latency, most reliable, runs on any machine · ~$0.03/min"},
	{Label: "Local speech-to-text", Hint: "transcription never leaves the machine · installs ~700 MB"},
	{Label: "Local text-to-speech", Hint: "fastest first audio on a decent CPU · installs ~600 MB"},
	{Label: "Fully local audio", Hint: "no audio leaves the machine, no per-minute audio cost · installs ~700 MB"},
}

// Where the local tiers run. Factor's own server is the default because it is
// the one that needs nothing from the user; the escape hatch is for a machine
// that already has Speaches or the like answering.
var speechHosts = []Option{
	{Label: "Let Factor install it (recommended)", Hint: "downloads the engines and the models for your language, once"},
	{Label: "I already run a speech server", Hint: "any OpenAI-compatible endpoint — Speaches, whisper-server, …"},
}

const defaultSpeechServer = "http://127.0.0.1:8000/v1"

func (w *wiz) stepPhone(ctx context.Context) error {
	w.ui.Note("Factor can also answer the phone and text you — a real number, your voice, its memory")
	existing := phoneConfig(w.cfg)
	want, err := w.ui.Confirm("Set up phone calls and SMS?", existing.PhoneNumber != "")
	if err != nil {
		return err
	}
	if !want {
		return w.askKeepChannel("phone", "The phone channel")
	}

	section, err := w.askCarrier(ctx, existing)
	if err != nil || section == nil {
		return err
	}
	if err := w.askVoiceTier(ctx, section, existing); err != nil {
		return err
	}
	if err := w.askProactive(section, existing); err != nil {
		return err
	}

	raw, err := json.Marshal(section)
	if err != nil {
		return err
	}
	if w.cfg.Channels == nil {
		w.cfg.Channels = map[string]json.RawMessage{}
	}
	w.cfg.Channels["phone"] = raw
	w.ui.Note("only %s can reach the agent by phone; add more with channels.phone.allow_from", section.UserNumber)
	w.ui.Note("the voice shell (Patter) installs itself into its own virtualenv on first start")
	return nil
}

// The two carriers the voice shell speaks. They differ in what they cost and
// in how much setup they need, which is the whole of the choice.
var carrierChoices = []Option{
	{Label: "Twilio (recommended)", Hint: "least setup: Factor points the number at itself on every start"},
	{Label: "Telnyx", Hint: "cheaper per minute and per text · needs a Call Control Application"},
}

const (
	carrierTwilio = "twilio"
	carrierTelnyx = "telnyx"
)

// askCarrier collects and verifies the telephony credentials. A nil section
// means the user backed out.
func (w *wiz) askCarrier(ctx context.Context, existing phoneSection) (*phoneSection, error) {
	idx, err := w.ui.Select("Which carrier is the number at?", carrierChoices, carrierIndex(existing))
	if err != nil {
		return nil, err
	}
	section := &phoneSection{Carrier: carrierTwilio}
	if idx == 1 {
		section.Carrier = carrierTelnyx
	}

	if section.Carrier == carrierTelnyx {
		err = w.askTelnyx(ctx, section, existing)
	} else {
		err = w.askTwilio(ctx, section, existing)
	}
	if err != nil || section.Carrier == "" {
		return nil, err
	}

	number, err := w.ui.Input("The number you bought, in E.164 (e.g. +15550001234)", existing.PhoneNumber)
	if err != nil {
		return nil, err
	}
	mine, err := w.ui.Input("Your own number, in E.164 — the only one allowed to call in", existing.UserNumber)
	if err != nil {
		return nil, err
	}
	section.PhoneNumber = strings.TrimSpace(number)
	section.UserNumber = strings.TrimSpace(mine)
	return section, nil
}

// carrierIndex preselects the carrier already configured.
func carrierIndex(existing phoneSection) int {
	if existing.Carrier == carrierTelnyx {
		return 1
	}
	return 0
}

// askTwilio fills in the Twilio half. Clearing the section's carrier is how it
// says the user gave no credentials and the phone should be skipped.
func (w *wiz) askTwilio(ctx context.Context, section *phoneSection, existing phoneSection) error {
	w.ui.Note("buy a number at twilio.com, then paste the account SID and auth token from its console")
	sid, err := w.ui.Input("Twilio account SID", existing.TwilioAccountSID)
	if err != nil {
		return err
	}
	token, err := w.ui.Secret("Twilio auth token", existing.TwilioAuthToken)
	if err != nil {
		return err
	}
	sid, token = strings.TrimSpace(sid), strings.TrimSpace(token)
	if sid == "" || token == "" {
		w.ui.Warn("no carrier credentials — skipping the phone")
		section.Carrier = ""
		return nil
	}
	var account string
	_ = w.ui.Task("verifying the Twilio credentials", func() error {
		var err error
		account, err = CheckTwilio(ctx, w.opts.HTTP, w.opts.Twilio, sid, token)
		return err
	})
	if account != "" {
		w.ui.Success("connected to the %q account", account)
	}
	section.TwilioAccountSID, section.TwilioAuthToken = sid, token
	return nil
}

// askTelnyx fills in the Telnyx half. The public key is not optional here: the
// voice shell refuses webhooks it cannot verify, so a call without one never
// connects.
func (w *wiz) askTelnyx(ctx context.Context, section *phoneSection, existing phoneSection) error {
	w.ui.Note("in the Telnyx portal: create a Call Control Application and buy a number — Factor attaches the two itself")
	w.ui.Note("the API key is under Auth → API Keys; the public key is on the same page, and the connection id is the application's")
	key, err := w.ui.Secret("Telnyx API key", existing.TelnyxAPIKey)
	if err != nil {
		return err
	}
	connection, err := w.ui.Input("Telnyx connection id (the Call Control Application's id)", existing.TelnyxConnectionID)
	if err != nil {
		return err
	}
	key, connection = strings.TrimSpace(key), strings.TrimSpace(connection)
	if key == "" || connection == "" {
		w.ui.Warn("no carrier credentials — skipping the phone")
		section.Carrier = ""
		return nil
	}
	var application string
	_ = w.ui.Task("verifying the Telnyx credentials", func() error {
		var err error
		application, err = CheckTelnyx(ctx, w.opts.HTTP, w.opts.Telnyx, key, connection)
		return err
	})
	if application != "" {
		w.ui.Success("connected to the %q application", application)
	}

	public, err := w.ui.Secret("Telnyx public key (Auth → API Keys → Public Key)", existing.TelnyxPublicKey)
	if err != nil {
		return err
	}
	public = strings.TrimSpace(public)
	if public == "" {
		w.ui.Warn("without the public key the shell refuses every call webhook — skipping the phone")
		section.Carrier = ""
		return nil
	}
	section.TelnyxAPIKey, section.TelnyxConnectionID, section.TelnyxPublicKey = key, connection, public
	w.ui.Note("Factor points the application's webhook at itself on every start, so the tunnel can move")
	return nil
}

func (w *wiz) askVoiceTier(ctx context.Context, section *phoneSection, existing phoneSection) error {
	language, err := w.ui.Input("Language on the call (BCP-47, e.g. en or es)", firstNonEmpty(existing.Language, "en"))
	if err != nil {
		return err
	}
	section.Language = strings.TrimSpace(language)

	idx, err := w.ui.Select("How should speech be handled?", voiceTiers, tierIndex(existing))
	if err != nil {
		return err
	}
	localSTT := idx == 1 || idx == 3
	localTTS := idx == 2 || idx == 3

	if localSTT || localTTS {
		if err := w.setUpLocalSpeech(ctx, section, existing, localSTT, localTTS); err != nil {
			return err
		}
	}

	if !localSTT {
		w.ui.Note("transcription runs on Deepgram (nova-3, ~$0.008/min); get a key at console.deepgram.com")
		key, err := w.ui.Secret("Deepgram API key", existing.STTAPIKey)
		if err != nil {
			return err
		}
		section.STT = audioSection{Provider: "deepgram"}
		section.STTAPIKey = strings.TrimSpace(key)
		if section.STTAPIKey == "" {
			w.ui.Warn("without a transcription key the agent cannot hear anything")
		}
	}
	if !localTTS {
		w.ui.Note("the voice is ElevenLabs flash v2.5 (~75 ms to first audio); get a key at elevenlabs.io")
		key, err := w.ui.Secret("ElevenLabs API key", existing.ElevenLabsAPIKey)
		if err != nil {
			return err
		}
		section.TTS = audioSection{Provider: "elevenlabs"}
		section.ElevenLabsAPIKey = strings.TrimSpace(key)
		if section.ElevenLabsAPIKey == "" {
			w.ui.Warn("without a voice key the agent cannot speak")
		} else {
			var plan string
			_ = w.ui.Task("verifying the ElevenLabs key", func() error {
				var err error
				plan, err = CheckElevenLabs(ctx, w.opts.HTTP, w.opts.ElevenLabs, section.ElevenLabsAPIKey)
				return err
			})
			if plan != "" {
				w.ui.Info("ElevenLabs plan: %s", plan)
			}
			voice, err := w.ui.Input("Voice id (blank = the default voice)", existing.VoiceID)
			if err != nil {
				return err
			}
			section.VoiceID = strings.TrimSpace(voice)
		}
	}
	return nil
}

// setUpLocalSpeech gets the local half of the pipeline actually working before
// the wizard moves on: the engines installed, the models for this language on
// disk, and the endpoints pointed at whatever will serve them. The one thing
// it will not do is leave the user with a config that only works once they go
// and start something themselves.
func (w *wiz) setUpLocalSpeech(ctx context.Context, section *phoneSection, existing phoneSection,
	localSTT, localTTS bool) error {

	byo := existing.STT.BaseURL != "" || existing.TTS.BaseURL != ""
	host, err := w.ui.Select("Where should local speech run?", speechHosts, boolIndex(byo))
	if err != nil {
		return err
	}

	if host == 0 {
		return w.installLocalSpeech(ctx, section, localSTT, localTTS)
	}

	base, err := w.ui.Input("Local speech server base URL",
		firstNonEmpty(existing.STT.BaseURL, existing.TTS.BaseURL, defaultSpeechServer))
	if err != nil {
		return err
	}
	base = strings.TrimSpace(base)
	if err := w.ui.Task("checking the local speech server", func() error {
		return CheckSpeechServer(ctx, w.opts.HTTP, base)
	}); err != nil {
		w.ui.Note("start it before the first call — Factor falls back to the cloud tier until it answers")
	}
	if localSTT {
		model, err := w.ui.Input("Speech-to-text model (blank = the server's default)", existing.STT.Model)
		if err != nil {
			return err
		}
		section.STT = audioSection{Provider: "local-openai", BaseURL: base, Model: strings.TrimSpace(model)}
	}
	if localTTS {
		voice, err := w.ui.Input("Voice (blank = the server's default)", existing.TTS.Voice)
		if err != nil {
			return err
		}
		section.TTS = audioSection{Provider: "local-openai", BaseURL: base, Voice: strings.TrimSpace(voice)}
	}
	return nil
}

// installLocalSpeech puts the engines and the models on the machine and
// records what the installer picked, so the first call finds the weights
// already there and the choices already made.
func (w *wiz) installLocalSpeech(ctx context.Context, section *phoneSection, localSTT, localTTS bool) error {
	// The endpoints stay blank on purpose: that is what marks the server as
	// Factor's own, and it lets the port move later without rewriting them.
	if localSTT {
		section.STT = audioSection{Provider: "local-openai"}
	}
	if localTTS {
		section.TTS = audioSection{Provider: "local-openai"}
	}

	w.ui.Note("Factor installs the speech engines into their own virtualenv and downloads the models for %q",
		section.Language)
	if w.opts.NoInstall {
		w.ui.Note("skipping the download; Factor installs it on the first start instead")
		return nil
	}

	var choices phone.SpeechChoices
	err := w.ui.Task("installing local speech (this takes a few minutes the first time)", func() error {
		var installErr error
		choices, installErr = w.opts.InstallSpeech(ctx, section.Language, localSTT, localTTS, w.ui.Progress())
		return installErr
	})
	if err != nil {
		// A failed install is not a failed setup: the config is already
		// correct, and the gateway retries the install when it starts.
		w.ui.Warn("the local speech install did not finish — Factor will try again when the gateway starts")
		return nil
	}

	section.SpeechServer = &speechSection{
		WhisperModel:   choices.WhisperModel,
		WhisperDevice:  choices.WhisperDevice,
		WhisperCompute: choices.WhisperCompute,
		PiperVoice:     choices.PiperVoice,
	}
	w.ui.Success("local speech ready — %s", choices.Summary())
	if choices.Warning != "" {
		w.ui.Warn("%s", choices.Warning)
	}
	return nil
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

// tierIndex maps an existing section back onto the tier menu.
func tierIndex(existing phoneSection) int {
	return tierIndexFor(existing.STT.Provider, existing.TTS.Provider)
}

func tierIndexFor(sttProvider, ttsProvider string) int {
	localSTT := sttProvider == "local-openai"
	localTTS := ttsProvider == "local-openai"
	switch {
	case localSTT && localTTS:
		return 3
	case localTTS:
		return 2
	case localSTT:
		return 1
	default:
		return 0
	}
}

var proactiveModes = []Option{
	{Label: "Text me", Hint: "an SMS — quiet, cheap, and it waits for you (default)"},
	{Label: "Call me", Hint: "it rings you; falls back to a text if the call cannot be placed"},
	{Label: "Stay quiet", Hint: "nothing reaches you by phone unless you call in"},
}

func (w *wiz) askProactive(section *phoneSection, existing phoneSection) error {
	def := 0
	switch existing.Proactive {
	case "call":
		def = 1
	case "off":
		def = 2
	}
	idx, err := w.ui.Select("When Factor needs you and you are not in a chat, it should…", proactiveModes, def)
	if err != nil {
		return err
	}
	section.Proactive = []string{"sms", "call", "off"}[idx]
	return nil
}

// ---- step 3c: PC voice -----------------------------------------------------

// voiceSection mirrors channels.voice, the way phoneSection mirrors the phone.
type voiceSection struct {
	Language    string `json:"language,omitempty"`
	Activation  string `json:"activation,omitempty"`
	WakeWord    string `json:"wake_word,omitempty"`
	InputDevice string `json:"input_device,omitempty"`

	STT              audioSection `json:"stt,omitempty"`
	STTAPIKey        string       `json:"stt_api_key,omitempty"`
	TTS              audioSection `json:"tts,omitempty"`
	ElevenLabsAPIKey string       `json:"elevenlabs_api_key,omitempty"`
	VoiceID          string       `json:"voice_id,omitempty"`

	SpeechServer *speechSection `json:"speech_server,omitempty"`
}

func voiceChannelConfig(cfg *config.Config) voiceSection {
	var section voiceSection
	if raw, ok := cfg.Channels["voice"]; ok {
		_ = json.Unmarshal(raw, &section)
	}
	return section
}

func (s voiceSection) configured() bool {
	return s.Activation != "" || s.STT.Provider != "" || s.STTAPIKey != ""
}

// The activation modes, in the words the config uses.
var activationChoices = []Option{
	{Label: "Always listening", Hint: "every utterance is a request — best in a quiet room"},
	{Label: "Wake word", Hint: "only utterances that open with the wake word (plus a short follow-up window)"},
	{Label: "Push-to-talk", Hint: "only after `factor talk` arms the microphone"},
}

var activationValues = []string{"always", "wake-word", "push-to-talk"}

func activationIndex(existing voiceSection) int {
	for i, value := range activationValues {
		if value == existing.Activation {
			return i
		}
	}
	return 1 // wake word: the default that does not answer the television
}

func (w *wiz) stepVoice(ctx context.Context) error {
	env := w.opts.Audio
	if !voice.MachineHasAudio(env) {
		w.ui.Note("no sound system detected — skipping PC voice (mic + speakers)")
		return nil
	}
	existing := voiceChannelConfig(w.cfg)
	w.ui.Note("Factor can also listen on this machine's microphone and answer through its speakers")
	want, err := w.ui.Confirm("Set up PC voice?", existing.configured())
	if err != nil {
		return err
	}
	if !want {
		return w.askKeepChannel("voice", "PC voice")
	}

	if err := w.installAudioHelpers(ctx, env); err != nil {
		return err
	}

	section := &voiceSection{}
	if err := w.askMicrophone(ctx, section, existing); err != nil {
		return err
	}
	phoneExisting := phoneConfig(w.cfg)
	language, err := w.ui.Input("Language spoken at the machine (BCP-47, e.g. en or es)",
		firstNonEmpty(existing.Language, phoneExisting.Language, "en"))
	if err != nil {
		return err
	}
	section.Language = strings.TrimSpace(language)

	if err := w.askVoiceSpeechTier(ctx, section, existing, phoneExisting); err != nil {
		return err
	}
	if err := w.askActivation(section, existing); err != nil {
		return err
	}

	raw, err := json.Marshal(section)
	if err != nil {
		return err
	}
	if w.cfg.Channels == nil {
		w.cfg.Channels = map[string]json.RawMessage{}
	}
	w.cfg.Channels["voice"] = raw
	w.ui.Note("the microphone opens whenever `factor` or `factor gateway` runs")
	return nil
}

// installAudioHelpers gets the capture and playback programs onto the
// machine, the way the desktop step installs its helpers: setup is where
// dependencies get met.
func (w *wiz) installAudioHelpers(ctx context.Context, env voice.Env) error {
	missing := voice.MissingHelpers(env)
	if len(missing) == 0 {
		w.ui.Success("audio helpers are installed")
		return nil
	}
	var names []string
	for _, h := range missing {
		names = append(names, fmt.Sprintf("%s (%s)", h.Bin, h.Purpose))
	}
	w.ui.Warn("missing audio helpers: %s", strings.Join(names, ", "))
	if w.opts.NoInstall {
		return nil
	}
	manager := tools.DetectSystemManager()
	if manager == "" {
		w.ui.Note("no supported package manager found — install them with your system's tools")
		return nil
	}
	packages := desktop.PackagesFor(missing, manager)
	install, err := w.ui.Confirm(fmt.Sprintf("Install %s with %s?", strings.Join(packages, " "), manager), true)
	if err != nil {
		return err
	}
	if !install {
		return nil
	}
	if err := w.ui.Task("installing audio helpers", func() error {
		_, err := w.opts.InstallPackages(ctx, packages)
		return err
	}); err != nil {
		w.ui.Note("install them yourself with: sudo %s install %s", manager, strings.Join(packages, " "))
	}
	return nil
}

// micCheckDuration is how long the live microphone check listens; a variable
// so tests do not sit through it.
var micCheckDuration = 2 * time.Second

// askMicrophone picks the capture device and proves it is alive — the check
// that would have caught a silent default source at setup instead of at the
// first ignored wake word. Machines whose sound server cannot list sources
// keep the default device; machines whose harness has no capture seam skip
// the live check.
func (w *wiz) askMicrophone(ctx context.Context, section *voiceSection, existing voiceSection) error {
	env := w.opts.Audio
	sources := voice.CaptureSources(ctx, env)
	device := existing.InputDevice

	choose := func() error {
		opts := make([]Option, 0, len(sources)+1)
		opts = append(opts, Option{Label: "System default", Hint: "whatever the sound server routes"})
		def := 0
		for i, source := range sources {
			opts = append(opts, Option{Label: source})
			if source == device {
				def = i + 1
			}
		}
		idx, err := w.ui.Select("Which microphone?", opts, def)
		if err != nil {
			return err
		}
		if idx == 0 {
			device = ""
		} else {
			device = sources[idx-1]
		}
		return nil
	}

	if len(sources) > 0 {
		if err := choose(); err != nil {
			return err
		}
	}
	for env.Capture != nil {
		w.ui.Note("make a little noise for the microphone check — tap the desk, say anything")
		var peak float64
		err := w.ui.Task("listening", func() error {
			var measureErr error
			peak, measureErr = voice.MeasureMic(ctx, env, device, micCheckDuration)
			return measureErr
		})
		if err == nil && peak > 0 {
			w.ui.Success("microphone is live (level %.0f)", peak)
			break
		}
		// Exactly zero is the wrong-device signature; an error is a helper
		// that could not open the source at all. Both mean this microphone
		// would never hear the wake word.
		w.ui.Warn("that source delivered no signal — the wrong device, or muted")
		if len(sources) == 0 {
			break
		}
		again, confirmErr := w.ui.Confirm("Pick a different source?", true)
		if confirmErr != nil {
			return confirmErr
		}
		if !again {
			break
		}
		if err := choose(); err != nil {
			return err
		}
	}
	section.InputDevice = device
	return nil
}

// askVoiceSpeechTier is the phone's speech-tier question asked for the PC:
// the same tiers, the same local install, its own section. Cloud keys default
// to whatever the phone step already collected, so a machine with both
// channels types each secret once.
func (w *wiz) askVoiceSpeechTier(ctx context.Context, section *voiceSection,
	existing voiceSection, phoneExisting phoneSection) error {

	idx, err := w.ui.Select("How should speech be handled?", voiceTiers,
		tierIndexFor(existing.STT.Provider, existing.TTS.Provider))
	if err != nil {
		return err
	}
	localSTT := idx == 1 || idx == 3
	localTTS := idx == 2 || idx == 3

	if localSTT || localTTS {
		if err := w.setUpLocalVoiceSpeech(ctx, section, existing, localSTT, localTTS); err != nil {
			return err
		}
	}

	if !localSTT {
		w.ui.Note("transcription runs on Deepgram (nova-3); get a key at console.deepgram.com")
		key, err := w.ui.Secret("Deepgram API key", firstNonEmpty(existing.STTAPIKey, phoneExisting.STTAPIKey))
		if err != nil {
			return err
		}
		section.STT = audioSection{Provider: "deepgram"}
		section.STTAPIKey = strings.TrimSpace(key)
		if section.STTAPIKey == "" {
			w.ui.Warn("without a transcription key the agent cannot hear anything")
		}
	}
	if !localTTS {
		w.ui.Note("the voice is ElevenLabs flash v2.5; get a key at elevenlabs.io")
		key, err := w.ui.Secret("ElevenLabs API key", firstNonEmpty(existing.ElevenLabsAPIKey, phoneExisting.ElevenLabsAPIKey))
		if err != nil {
			return err
		}
		section.TTS = audioSection{Provider: "elevenlabs"}
		section.ElevenLabsAPIKey = strings.TrimSpace(key)
		if section.ElevenLabsAPIKey == "" {
			w.ui.Warn("without a voice key the agent cannot speak")
		} else {
			var plan string
			_ = w.ui.Task("verifying the ElevenLabs key", func() error {
				var err error
				plan, err = CheckElevenLabs(ctx, w.opts.HTTP, w.opts.ElevenLabs, section.ElevenLabsAPIKey)
				return err
			})
			if plan != "" {
				w.ui.Info("ElevenLabs plan: %s", plan)
			}
			voiceID, err := w.ui.Input("Voice id (blank = the default voice)",
				firstNonEmpty(existing.VoiceID, phoneExisting.VoiceID))
			if err != nil {
				return err
			}
			section.VoiceID = strings.TrimSpace(voiceID)
		}
	}
	return nil
}

// setUpLocalVoiceSpeech mirrors the phone's setUpLocalSpeech onto the voice
// section: Factor's own server by default, an existing one by base URL.
func (w *wiz) setUpLocalVoiceSpeech(ctx context.Context, section *voiceSection, existing voiceSection,
	localSTT, localTTS bool) error {

	byo := existing.STT.BaseURL != "" || existing.TTS.BaseURL != ""
	host, err := w.ui.Select("Where should local speech run?", speechHosts, boolIndex(byo))
	if err != nil {
		return err
	}

	if host == 0 {
		// Blank endpoints mark the server as Factor's own; the voice channel
		// serves it on its own port, clear of the phone's.
		if localSTT {
			section.STT = audioSection{Provider: "local-openai"}
		}
		if localTTS {
			section.TTS = audioSection{Provider: "local-openai"}
		}
		w.ui.Note("Factor installs the speech engines into their own virtualenv and downloads the models for %q",
			section.Language)
		if w.opts.NoInstall {
			w.ui.Note("skipping the download; Factor installs it on the first start instead")
			return nil
		}
		var choices phone.SpeechChoices
		err := w.ui.Task("installing local speech (this takes a few minutes the first time)", func() error {
			var installErr error
			choices, installErr = w.opts.InstallSpeech(ctx, section.Language, localSTT, localTTS, w.ui.Progress())
			return installErr
		})
		if err != nil {
			w.ui.Warn("the local speech install did not finish — Factor will try again when it starts")
			return nil
		}
		section.SpeechServer = &speechSection{
			WhisperModel:   choices.WhisperModel,
			WhisperDevice:  choices.WhisperDevice,
			WhisperCompute: choices.WhisperCompute,
			PiperVoice:     choices.PiperVoice,
		}
		w.ui.Success("local speech ready — %s", choices.Summary())
		if choices.Warning != "" {
			w.ui.Warn("%s", choices.Warning)
		}
		return nil
	}

	base, err := w.ui.Input("Local speech server base URL",
		firstNonEmpty(existing.STT.BaseURL, existing.TTS.BaseURL, defaultSpeechServer))
	if err != nil {
		return err
	}
	base = strings.TrimSpace(base)
	if err := w.ui.Task("checking the local speech server", func() error {
		return CheckSpeechServer(ctx, w.opts.HTTP, base)
	}); err != nil {
		w.ui.Note("start it before talking — Factor falls back to the cloud tier until it answers")
	}
	if localSTT {
		model, err := w.ui.Input("Speech-to-text model (blank = the server's default)", existing.STT.Model)
		if err != nil {
			return err
		}
		section.STT = audioSection{Provider: "local-openai", BaseURL: base, Model: strings.TrimSpace(model)}
	}
	if localTTS {
		voiceName, err := w.ui.Input("Voice (blank = the server's default)", existing.TTS.Voice)
		if err != nil {
			return err
		}
		section.TTS = audioSection{Provider: "local-openai", BaseURL: base, Voice: strings.TrimSpace(voiceName)}
	}
	return nil
}

func (w *wiz) askActivation(section *voiceSection, existing voiceSection) error {
	idx, err := w.ui.Select("When should Factor respond?", activationChoices, activationIndex(existing))
	if err != nil {
		return err
	}
	section.Activation = activationValues[idx]
	if section.Activation == "wake-word" {
		wake, err := w.ui.Input("Wake word", firstNonEmpty(existing.WakeWord, "factor"))
		if err != nil {
			return err
		}
		section.WakeWord = strings.TrimSpace(wake)
		w.ui.Note("`factor talk` still arms the microphone directly, for when the wake word misfires")
	}
	if section.Activation == "push-to-talk" {
		w.ui.Note("arm the microphone with `factor talk`")
	}
	return nil
}

// ---- step 4: desktop and tools --------------------------------------------

func (w *wiz) stepDesktop(ctx context.Context) error {
	w.ui.Step(4, totalSteps, "Tools")

	env := w.opts.Desktop
	ctl := desktop.NewController(env)
	if !desktop.MachineHasDisplay(env) {
		w.ui.Note("no graphical session detected — desktop tools stay off (desktop.enabled forces them on)")
	} else {
		w.ui.Info("desktop backend: %s", ctl.Backend())
		if !desktop.HasDisplay(env) {
			w.ui.Note("this session has no DISPLAY, but the machine is running one — setting the tools up for it")
		}
		enable, err := w.ui.Confirm("Enable desktop control (windows, screenshots, mouse, keyboard, clipboard)?", true)
		if err != nil {
			return err
		}
		w.cfg.Desktop.Enabled = &enable
		if enable {
			if err := w.installDesktopHelpers(ctx, env, ctl); err != nil {
				return err
			}
		}
	}

	restrict, err := w.ui.Confirm("Restrict file tools to the workspace? (safer)", w.cfg.Tools.RestrictToWorkspace)
	if err != nil {
		return err
	}
	w.cfg.Tools.RestrictToWorkspace = restrict
	if !restrict {
		w.ui.Warn("file tools can now read and write anywhere your user can")
	}

	return w.setupBrowser(ctx, env)
}

// setupBrowser leaves the machine with a browser the agent can actually
// drive. Asking "enable the browser tools?" and taking yes for an answer was
// how users ended up with the tools registered and no browser to register
// them against — the question is now the start of the work, not all of it.
func (w *wiz) setupBrowser(ctx context.Context, env desktop.Env) error {
	// A -tags nobrowser build has no suite to enable, so asking about one —
	// let alone offering to download 120MB for it — would be theatre.
	if !browser.Available() {
		w.cfg.Browser.Enabled = false
		w.ui.Note("this build was made without the browser suite (-tags nobrowser)")
		return nil
	}
	enable, err := w.ui.Confirm("Enable the browser tools (a real browser the agent drives over DevTools)?", w.cfg.Browser.Enabled)
	if err != nil {
		return err
	}
	w.cfg.Browser.Enabled = enable
	if !enable {
		return nil
	}

	// Chrome refuses to start as root without --no-sandbox: not something
	// anyone should have to learn from a failed tool call. The other way a
	// working browser still fails to start — no display — depends on what
	// launches Factor later, so the browser decides that one itself.
	if geteuid() == 0 {
		w.cfg.Browser.NoSandbox = true
	}
	if !desktop.HasDisplay(env) {
		w.ui.Note("no display here — the browser runs headless unless whatever starts Factor has one")
	}

	path, err := browser.FindBrowserBinary(w.cfg.Browser.Command)
	if err != nil {
		if path, err = w.provisionBrowser(ctx); err != nil {
			return err
		}
		if path == "" {
			return nil
		}
	}
	w.cfg.Browser.Command = path
	w.ui.Success("browser: %s", path)

	if err := w.ui.Task("loading a test page", func() error {
		return w.opts.VerifyBrowser(ctx, w.cfg.Browser)
	}); err != nil {
		w.ui.Note("the browser is configured but did not finish a page here; the tools will try again on their first call")
	}
	return w.setupFastBrowser(ctx)
}

// setupFastBrowser offers the second engine. It stays off unless asked for:
// it is another browser to download, it cannot click or screenshot, and the
// real one already reads pages — it just costs far more memory to do it.
func (w *wiz) setupFastBrowser(ctx context.Context) error {
	if w.opts.NoInstall {
		return nil
	}
	// Do not offer what this machine cannot run: the answer would cost a
	// 150MB download to reach.
	if ok, why := w.opts.FastBrowserSupported(); !ok {
		w.cfg.Browser.FastPath = false
		w.ui.Note("%s — skipping it; the full browser reads pages fine", why)
		return nil
	}
	add, err := w.ui.Confirm("Also add a lightweight read-only engine for cheap page reads (Lightpanda, ~150 MB)?", w.cfg.Browser.FastPath)
	if err != nil {
		return err
	}
	if !add {
		w.cfg.Browser.FastPath = false
		return nil
	}
	progress := w.ui.Progress()
	var path string
	if err := w.ui.Task("installing Lightpanda", func() error {
		p, _, err := w.opts.EnsureFastBrowser(ctx, progress)
		path = p
		return err
	}); err != nil {
		w.cfg.Browser.FastPath = false
		w.ui.Note("the full browser handles page reads on its own; nothing else changes")
		return nil
	}
	w.cfg.Browser.FastPath = true
	w.cfg.Browser.FastCommand = path
	return nil
}

// provisionBrowser installs one, returning "" when the machine is left
// without a browser for a reason the user already heard about.
func (w *wiz) provisionBrowser(ctx context.Context) (string, error) {
	w.ui.Warn("no browser found — the agent cannot browse without one")
	if w.opts.NoInstall {
		return "", nil
	}
	install, err := w.ui.Confirm("Install Helium (privacy-patched Chromium, ~125 MB, no root needed)?", true)
	if err != nil {
		return "", err
	}
	if !install {
		w.ui.Note("the tools stay on and will pick up a browser as soon as one is installed")
		return "", nil
	}
	progress := w.ui.Progress()
	var path string
	if err := w.ui.Task("installing Helium", func() error {
		p, _, err := w.opts.EnsureBrowser(ctx, progress)
		path = p
		return err
	}); err != nil {
		w.ui.Note("install Chrome, Chromium, or Helium (https://helium.computer) and re-run factor init")
		return "", nil
	}
	return path, nil
}

func (w *wiz) installDesktopHelpers(ctx context.Context, env desktop.Env, ctl desktop.Controller) error {
	missing := desktop.MissingHelpers(env, ctl)
	if len(missing) == 0 {
		w.ui.Success("all desktop helpers are installed")
		return nil
	}
	var names []string
	for _, h := range missing {
		names = append(names, fmt.Sprintf("%s (%s)", h.Bin, h.Purpose))
	}
	w.ui.Warn("missing desktop helpers: %s", strings.Join(names, ", "))
	if w.opts.NoInstall {
		return nil
	}
	manager := tools.DetectSystemManager()
	if manager == "" {
		w.ui.Note("no supported package manager found — install them with your distribution's tools")
		return nil
	}
	packages := desktop.PackagesFor(missing, manager)
	install, err := w.ui.Confirm(fmt.Sprintf("Install %s with %s?", strings.Join(packages, " "), manager), true)
	if err != nil {
		return err
	}
	if !install {
		return nil
	}
	err = w.ui.Task("installing desktop helpers", func() error {
		_, err := w.opts.InstallPackages(ctx, packages)
		return err
	})
	if err != nil {
		w.ui.Note("install them yourself with: sudo %s install %s", manager, strings.Join(packages, " "))
	}
	return nil
}

// ---- step 5: save ----------------------------------------------------------

func (w *wiz) stepFinish(context.Context) error {
	w.ui.Step(5, totalSteps, "Saving")

	if err := config.EnsureWorkspace(w.cfg.Agent.Workspace); err != nil {
		return err
	}
	w.ui.Success("workspace ready at %s", w.cfg.Agent.Workspace)
	if err := w.cfg.Save(); err != nil {
		return err
	}
	w.ui.Success("config written to %s", w.cfg.Path())

	rows := [][2]string{
		{"provider", fmt.Sprintf("%s · %s", w.cfg.Provider.Type, w.cfg.Provider.Model)},
		{"reasoning", reasoningSummary(w.cfg.Provider.Reasoning)},
		{"memory", w.memorySummary()},
		{"channels", w.channelSummary()},
		{"desktop", w.desktopSummary()},
		{"browser", enabledLabel(w.cfg.Browser.Enabled)},
		{"workspace", w.cfg.Agent.Workspace},
	}
	w.ui.Summary("Factor is ready", rows)
	w.ui.printf("\n  %s   factor            %s\n", w.ui.style(ansiCyan, "▸"), w.ui.style(ansiDim, "chat right here"))
	w.ui.printf("  %s   factor gateway    %s\n", w.ui.style(ansiCyan, "▸"), w.ui.style(ansiDim, "run the daemon (channels, cron, heartbeat)"))
	w.ui.printf("  %s   factor status     %s\n\n", w.ui.style(ansiCyan, "▸"), w.ui.style(ansiDim, "check on it"))
	return nil
}

func (w *wiz) memorySummary() string {
	switch w.cfg.Memory.Mode {
	case "off":
		return "off"
	case "external":
		return "external · " + w.cfg.Memory.BaseURL()
	default:
		return fmt.Sprintf("sidecar · %s · port %d", w.cfg.Memory.Personality, w.cfg.Memory.Port)
	}
}

func (w *wiz) channelSummary() string {
	names := make([]string, 0, len(w.cfg.Channels))
	for name := range w.cfg.Channels {
		switch name {
		case "phone":
			name += " · " + voiceTierSummary(phoneConfig(w.cfg))
		case "voice":
			section := voiceChannelConfig(w.cfg)
			name += " · " + firstNonEmpty(section.Activation, "always") + " · " +
				strings.ToLower(voiceTiers[tierIndexFor(section.STT.Provider, section.TTS.Provider)].Label)
		}
		names = append(names, name)
	}
	sort.Strings(names) // map order would otherwise reshuffle the summary each run
	return strings.Join(append([]string{"cli"}, names...), ", ")
}

// voiceTierSummary names the speech tier in the words the tier menu used.
func voiceTierSummary(section phoneSection) string {
	return strings.ToLower(voiceTiers[tierIndex(section)].Label)
}

func (w *wiz) desktopSummary() string {
	if w.cfg.Desktop.Enabled == nil {
		return "auto"
	}
	return enabledLabel(*w.cfg.Desktop.Enabled)
}

func reasoningSummary(r config.ReasoningConfig) string {
	switch {
	case r.MaxTokens > 0:
		return fmt.Sprintf("%d thinking tokens", r.MaxTokens)
	case r.Effort == "" || r.Effort == "none":
		return "off"
	case r.Exclude:
		return r.Effort + " (hidden)"
	default:
		return r.Effort
	}
}

func enabledLabel(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return true
		}
	}
	return false
}
