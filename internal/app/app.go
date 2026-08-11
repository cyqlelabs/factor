// Package app is the composition root: it wires config, provider chain,
// smrti memory, tools, skills, sessions, and the agent loop into one unit
// shared by the CLI and the gateway daemon.
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/browser"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/cron"
	"github.com/cyqlelabs/factor/internal/jobs"
	"github.com/cyqlelabs/factor/internal/mcp"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/provider"
	"github.com/cyqlelabs/factor/internal/session"
	"github.com/cyqlelabs/factor/internal/skills"
	"github.com/cyqlelabs/factor/internal/tools"
)

type App struct {
	Cfg      *config.Config
	Bus      *bus.MessageBus
	Chain    *provider.Chain
	Memory   memory.Engine
	Ambient  *memory.Ambient
	Registry *tools.Registry
	Sessions *session.Store
	Skills   *skills.Loader
	Loop     *agent.Loop
	Jobs     *jobs.Engine
	Cron     *cron.Service
	MCP      *mcp.Manager

	closeBrowser func()
}

// New assembles a fully wired Factor instance. The memory sidecar starts in
// the background; health flips asynchronously.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
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

	extract := memory.DeriveExtract(cfg.Memory, cfg.Provider)
	engine, err := memory.NewEngine(ctx, cfg.Memory, extract, filepath.Join(config.Home(), "logs"))
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	ambient := memory.NewAmbient(engine,
		cfg.Memory.RecallTopK, cfg.Memory.RecallMinConfidence,
		cfg.Memory.QueryContextMsgs, cfg.Memory.QueryMaxChars, cfg.Memory.InjectMaxChars,
		cfg.Memory.IgnorePatterns)

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
	registry.Register(memory.NewTools(engine)...)
	registry.Register(&skills.InstallTool{Root: filepath.Join(ws, "skills")})
	registry.Register(tools.NewConfigTools(cfg)...)
	registry.Register(tools.NewPkgInstallTool())

	closeBrowser := func() {}
	if cfg.Browser.Enabled {
		var browserTools []tools.Tool
		browserTools, closeBrowser = browser.NewTools(cfg.Browser, ws)
		registry.Register(browserTools...)
	}

	mcpManager := mcp.NewManager(registry, cfg)
	registry.Register(mcp.NewTools(mcpManager)...)
	go mcpManager.StartAll(ctx) // dead servers must not stall startup

	loader := skills.NewLoader(filepath.Join(ws, "skills"), filepath.Join(config.Home(), "skills"))
	builder := agent.NewContextBuilder(cfg, loader, ambient)
	b := bus.New()
	loop := agent.NewLoop(cfg, b, chain, registry, sessions, builder, ambient)

	// Background jobs: completion events re-enter the originating session as
	// inbound messages, so the agent proactively reports back to the user
	// (steering handles the case where that session is mid-turn).
	jobEngine := jobs.NewEngine(ctx, ws,
		loop.ProcessDirect,
		func(job *jobs.Job) {
			content := fmt.Sprintf(
				"[system] Background job %s (%s, %q) finished with state %s.\nOutput tail:\n%s\n\nReport the outcome to the user concisely.",
				job.ID, job.Kind, job.Description, job.State, job.OutputTail())
			b.PublishInbound(bus.InboundMessage{
				Channel: job.Origin.Channel,
				ChatID:  job.Origin.ChatID,
				Content: content,
				Time:    time.Now(),
			})
		})
	registry.Register(jobs.NewTools(jobEngine)...)

	cronService, err := cron.NewService(filepath.Join(ws, "cron"),
		func(ctx context.Context, job cron.Job) (string, error) {
			return loop.ProcessDirect(ctx, job.Message, "cron:"+job.ID)
		},
		func(channelName, chatID, content string) {
			b.PublishOutbound(bus.OutboundMessage{Channel: channelName, ChatID: chatID, Content: content})
		})
	if err != nil {
		return nil, fmt.Errorf("cron: %w", err)
	}
	registry.Register(&cron.Tool{Service: cronService})

	return &App{
		Cfg:      cfg,
		Bus:      b,
		Chain:    chain,
		Memory:   engine,
		Ambient:  ambient,
		Registry: registry,
		Sessions: sessions,
		Skills:   loader,
		Loop:     loop,
		Jobs:     jobEngine,
		Cron:     cronService,
		MCP:      mcpManager,

		closeBrowser: closeBrowser,
	}, nil
}

func (a *App) Close() {
	a.closeBrowser()
	if a.MCP != nil {
		a.MCP.CloseAll()
	}
	if a.Memory != nil {
		_ = a.Memory.Close()
	}
}
