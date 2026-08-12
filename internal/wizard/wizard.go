package wizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	UI       *UI
	Version  string
	HTTP     *http.Client
	Home     string // FACTOR_HOME (defaults to config.Home())
	Telegram string // Telegram API base (defaults to the real one)

	// NonInteractive skips every prompt: defaults are kept, smrti is
	// installed when missing, and the config is written as-is.
	NonInteractive bool
	// NoInstall suppresses installing smrti and desktop helpers.
	NoInstall bool

	EnsureSmrti     func(ctx context.Context, cfg config.MemoryConfig, progress memory.Progress) (path string, installed bool, err error)
	InstallPackages func(ctx context.Context, packages []string) (string, error)
	Desktop         desktop.Env
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
	if o.EnsureSmrti == nil {
		o.EnsureSmrti = func(ctx context.Context, cfg config.MemoryConfig, progress memory.Progress) (string, bool, error) {
			return memory.EnsureSmrti(ctx, cfg.Command, o.Home, true, progress)
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

	if w.cfg.Memory.Mode != "off" && !w.opts.NoInstall {
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
	if w.cfg.Provider.APIKey == "" && os.Getenv("FACTOR_PROVIDER_API_KEY") == "" {
		w.ui.printf("provider:  no API key — export FACTOR_PROVIDER_API_KEY or run `factor init` interactively\n")
	}
	return nil
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
	{Type: "openrouter", Label: "OpenRouter", Hint: "one key, every major model", Model: "google/gemini-pro-latest", NeedsKey: true, KeyURL: "https://openrouter.ai/keys"},
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

	existing := telegramConfig(w.cfg)
	want, err := w.ui.Confirm("Set up Telegram now?", existing.Token != "")
	if err != nil {
		return err
	}
	if !want {
		return nil
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

func telegramConfig(cfg *config.Config) telegramSection {
	var section telegramSection
	if raw, ok := cfg.Channels["telegram"]; ok {
		_ = json.Unmarshal(raw, &section)
	}
	return section
}

// ---- step 4: desktop and tools --------------------------------------------

func (w *wiz) stepDesktop(ctx context.Context) error {
	w.ui.Step(4, totalSteps, "Tools")

	env := w.opts.Desktop
	ctl := desktop.NewController(env)
	if !desktop.HasDisplay(env) {
		w.ui.Note("no graphical session detected — desktop tools stay off (desktop.enabled forces them on)")
	} else {
		w.ui.Info("desktop backend: %s", ctl.Backend())
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

	browserDefault := w.cfg.Browser.Enabled
	browser, err := w.ui.Confirm("Enable the browser tools (real Chrome/Chromium via DevTools)?", browserDefault)
	if err != nil {
		return err
	}
	w.cfg.Browser.Enabled = browser
	return nil
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
	names := []string{"cli"}
	for name := range w.cfg.Channels {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
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
