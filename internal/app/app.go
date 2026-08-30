// Package app is the composition root: it wires config, provider chain,
// smrti memory, tools, skills, sessions, and the agent loop into one unit
// shared by the CLI and the gateway daemon.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/browser"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/cost"
	"github.com/cyqlelabs/factor/internal/cron"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/jobs"
	"github.com/cyqlelabs/factor/internal/mcp"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/session"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
	"github.com/cyqlelabs/factor/internal/upgrade"
	"github.com/cyqlelabs/factor/internal/version"
)

type App struct {
	Cfg   *config.Config
	Bus   *bus.MessageBus
	Chain *provider.Chain
	// Cost prices every provider call the loop makes and holds the budget
	// caps; the CLI bar and the tray overview read their numbers from it.
	Cost     *cost.Meter
	Memory   memory.Engine
	Ambient  *memory.Ambient
	Registry *tools.Registry
	// Guard is the path rule every file-touching tool obeys; the gateway
	// hands it to connectors that read or write files themselves.
	Guard    *tools.PathGuard
	Sessions *session.Store
	Skills   *skills.Loader
	// Ask carries the agent's questions to the user: into the chat whose
	// turn is asking, with a desktop dialog as the fallback for turns no
	// chat is behind; `factor chat` points it at the terminal instead.
	Ask  *tools.AskTool
	Loop *agent.Loop
	Jobs *jobs.Engine
	Cron *cron.Service
	MCP  *mcp.Manager
	// Restart is how the upgrade tool reloads this process into the release
	// it just installed. Only a daemon fills it in.
	Restart *upgrade.Restarter
	// SmrtiUpgrade keeps the memory engine current, and is nil when memory is
	// off — there is then no engine to keep current.
	SmrtiUpgrade *upgrade.Smrti

	closeBrowser func()

	// Background work this App owns, on a context of its own so Close can
	// end it without depending on whoever passed the parent in. The price
	// refresh writes into ~/.factor, so a Close that did not wait would let
	// a process that believes it has shut down touch the disk afterwards.
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
}

// New assembles a fully wired Factor instance. The memory sidecar starts in
// the background; health flips asynchronously.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	// The one place both the daemon and a chat session pass through, so the
	// log level is settled here before anything below it logs a line.
	cfg.ApplyLogLevel()

	// Same reason, for the screen: a gateway started at login is handed no
	// DISPLAY, and everything that reaches the desktop below reads it from
	// the environment.
	if adopted := desktop.AdoptDisplay(desktop.DefaultEnv(), os.Setenv); adopted != "" {
		slog.Info("adopted the machine's screen", "display", adopted)
	}

	ws := cfg.Agent.Workspace
	if err := config.EnsureWorkspace(ws); err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}

	sessions, err := session.NewStore(filepath.Join(ws, "sessions"))
	if err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}

	chain, err := provider.BuildChain(cfg.Provider)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}

	// Everything the loop asks a provider goes through the meter, so
	// compaction and wrap-up calls are billed alongside the ones the user
	// can see, and a cap stops the turn before the next call is paid for.
	catalog := cost.NewCatalog(cfg.Cost, cfg.Provider.Candidates(), filepath.Join(config.Home(), "pricing.json"))
	meter := cost.NewMeter(chain, catalog, cost.NewLedger(filepath.Join(config.Home(), "usage.json")), cfg.Cost)

	extract := memory.DeriveExtract(cfg.Memory, cfg.Provider)
	engine, err := memory.NewEngine(ctx, cfg.Memory, extract, filepath.Join(config.Home(), "logs"))
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	spaces, err := memory.NewSpacePolicy(cfg.Memory.SpaceStrategy, cfg.Memory.Space, cfg.Memory.SystemSpace,
		cfg.Memory.SharedSpace)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	ambient := memory.NewAmbient(engine,
		cfg.Memory.RecallTopK, cfg.Memory.RecallMinConfidence,
		cfg.Memory.QueryContextMsgs, cfg.Memory.QueryMaxChars, cfg.Memory.InjectMaxChars,
		cfg.Memory.IgnorePatterns, spaces)

	registry := tools.NewRegistry(cfg.Tools.IsToolEnabled, cfg.FilterSecrets)
	guard := tools.NewPathGuard(ws, cfg.Tools.RestrictToWorkspace, cfg.Tools.AllowReadOutsideWorkspace, cfg.Tools.AllowPaths)
	registry.Register(tools.NewFSTools(guard)...)
	execTool, err := tools.NewExecTool(guard,
		time.Duration(cfg.Tools.ExecTimeoutSecs)*time.Second, cfg.Tools.EnableDenyPatterns, cfg.Tools.CustomDenyPatterns)
	if err != nil {
		return nil, fmt.Errorf("exec tool: %w", err)
	}
	registry.Register(execTool)
	registry.Register(tools.NewWebTools()...)
	registry.Register(memory.NewTools(engine, spaces)...)
	skillsRoot := filepath.Join(ws, "skills")
	skillLoader := skills.NewLoader(skillsRoot, filepath.Join(config.Home(), "skills"))
	skillReg := skills.NewRegistry(cfg.Tools.SkillRegistryURL)
	registry.Register(&skills.InstallTool{Root: skillsRoot, Registry: skillReg})
	registry.Register(&skills.WriteTool{Root: skillsRoot})
	registry.Register(&skills.RemoveTool{Root: skillsRoot})
	registry.Register(&skills.FindTool{Registry: skillReg, Installed: skillLoader})
	registry.Register(tools.NewConfigTools(cfg)...)
	registry.Register(cost.NewTool(meter)...)
	registry.Register(tools.NewPkgInstallTool())
	// A question needs somewhere to land: the chat whose turn is asking when
	// there is one (wired below, once the loop exists), the machine's screen
	// otherwise; a chat session swaps in its terminal instead (App.Ask).
	askEnv := tools.DefaultAskEnv()
	// The screen is re-checked at the moment the question is asked, not at
	// startup: the display adopted then dies with the session it belonged to,
	// and a dialog spawned at a dead one exits with the code that means the
	// user said no.
	askEnv.Display = desktop.ScreenReady
	dialogAsker := tools.NewDialogAsker(askEnv)
	askTool := tools.NewAskTool(dialogAsker)
	registry.Register(askTool)
	restarter := &upgrade.Restarter{}
	// The engine is upgraded in place, so what gates it is the graph being
	// idle rather than the process being about to exit.
	var smrtiUpgrade *upgrade.Smrti
	if cfg.Memory.Mode != "off" {
		smrtiUpgrade = upgrade.NewSmrti(cfg.Memory, memory.IdleFunc(engine, memory.UpgradeQuiet))
	}
	registry.Register(&upgrade.Tool{Current: version.Version, Restart: restarter, Smrti: smrtiUpgrade})

	// Desktop control: skipped on headless machines, where these tools would
	// be prompt weight that can only ever fail (desktop.enabled forces it).
	desktopEnv := desktop.DefaultEnv()
	if cfg.Desktop.Register(desktop.HasDisplay(desktopEnv)) {
		shotDir := cfg.Desktop.ScreenshotDir
		if shotDir == "" {
			shotDir = filepath.Join(ws, "screenshots")
		}
		registry.Register(desktop.NewTools(desktopEnv, guard, shotDir)...)
	} else {
		slog.Info("desktop tools not registered: no graphical session (set desktop.enabled=true to force)")
	}

	closeBrowser := func() {}
	if cfg.Browser.Enabled {
		var browserTools []tools.Tool
		browserTools, closeBrowser = browser.NewTools(cfg.Browser, ws, guard)
		registry.Register(browserTools...)
	}

	mcpManager := mcp.NewManager(registry, cfg)
	registry.Register(mcp.NewTools(mcpManager)...)
	go mcpManager.StartAll(ctx) // dead servers must not stall startup

	builder := agent.NewContextBuilder(cfg, skillLoader, ambient)
	b := bus.New()
	loop := agent.NewLoop(cfg, b, meter, registry, sessions, builder, ambient)
	// A turn that arrived from a chat asks its questions there — the user the
	// question is for is by definition looking at that chat, not necessarily
	// at this machine's screen. The dialog stays for every turn without one.
	askTool.SetAsker(loop.Asker(dialogAsker))
	// Compaction budgets against the tightest window among the models the
	// chain can serve a turn with — failover must not overflow the fallback.
	// A model the catalog cannot size (locally served ones, above all) does
	// not shrink the answer; the loop falls back to config for those.
	loop.SetContextWindow(func() int {
		window := 0
		for _, cand := range cfg.Provider.Candidates() {
			if w := catalog.Window(cand.Model); w > 0 && (window == 0 || w < window) {
				window = w
			}
		}
		return window
	})

	// Background jobs: completion events re-enter the originating session as
	// inbound messages, so the agent proactively reports back to the user
	// (steering handles the case where that session is mid-turn).
	jobEngine := jobs.NewEngine(ctx, ws,
		loop.ProcessDirect,
		func(job *jobs.Job) {
			v := job.Snapshot()
			content := fmt.Sprintf(
				"[system] Background job %s (%s, %q) finished with state %s.\nOutput tail:\n%s\n\nReport the outcome to the user concisely.",
				v.ID, v.Kind, v.Description, v.State, job.OutputTail())
			b.PublishInbound(bus.InboundMessage{
				Channel: v.Origin.Channel,
				ChatID:  v.Origin.ChatID,
				Content: content,
				Time:    time.Now(),
				System:  true, // never the user speaking: must not answer a standing ask_user
			})
		})
	registry.Register(jobs.NewTools(jobEngine)...)

	cronService, err := cron.NewService(filepath.Join(ws, "cron"),
		func(ctx context.Context, job cron.Job) (string, error) {
			return loop.ProcessDirect(ctx, job.Message, "cron:"+job.ID)
		},
		func(channelName, chatID, content string) {
			channelName, chatID = cronTarget(channelName, chatID, loop.LastChannel)
			b.PublishOutbound(bus.OutboundMessage{Channel: channelName, ChatID: chatID, Content: content})
		})
	if err != nil {
		return nil, fmt.Errorf("cron: %w", err)
	}
	registry.Register(&cron.Tool{Service: cronService})

	bgCtx, bgCancel := context.WithCancel(ctx)
	app := &App{
		Cfg:      cfg,
		Bus:      b,
		Chain:    chain,
		Cost:     meter,
		Memory:   engine,
		Ambient:  ambient,
		Registry: registry,
		Guard:    guard,
		Sessions: sessions,
		Skills:   skillLoader,
		Ask:      askTool,
		Loop:     loop,
		Jobs:     jobEngine,
		Cron:     cronService,
		MCP:      mcpManager,
		Restart:  restarter,

		SmrtiUpgrade: smrtiUpgrade,

		closeBrowser: closeBrowser,
		bgCancel:     bgCancel,
	}
	// The price catalog refreshes on a daily tick for as long as the App
	// lives. Started here rather than where the catalog is built so Close
	// owns it: it is the one background task that writes to disk on its own
	// schedule, and nothing else knows when it is done.
	if meter.Active() {
		app.bg.Add(1)
		go func() {
			defer app.bg.Done()
			catalog.Watch(bgCtx)
		}()
	}
	// Scoping recall by audience splits the graph in two, and the bridge is
	// what keeps that from costing recall quality: a subject discussed both
	// alone and in company would otherwise sit in two islands neither side's
	// graph expansion can cross. It waits for a gathering to end rather than
	// polling, and returns at once when there is nothing to bridge, so
	// starting it is unconditional.
	app.bg.Add(1)
	go func() {
		defer app.bg.Done()
		ambient.WatchBridges(bgCtx)
	}()
	return app, nil
}

// cronTarget resolves where a scheduled job's result should go. A job
// created outside a real conversation — from the CLI, from a heartbeat, from
// another cron job — has no chat of its own under the gateway, so it follows
// the user to the last external chat they used.
func cronTarget(channelName, chatID string, last func() (string, string, bool)) (string, string) {
	if bus.External(channelName) {
		return channelName, chatID
	}
	if ch, chat, ok := last(); ok {
		return ch, chat
	}
	return channelName, chatID
}

func (a *App) Close() {
	// First, because it is the only shutdown step with a deadline of its own
	// making: until the price refresh is off the disk, this process is still
	// writing files a caller may be about to delete.
	if a.bgCancel != nil {
		a.bgCancel()
		a.bg.Wait()
	}
	a.closeBrowser()
	if a.MCP != nil {
		a.MCP.CloseAll()
	}
	if a.Memory != nil {
		_ = a.Memory.Close()
	}
}
