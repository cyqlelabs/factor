// Command factor is a fast, reliable desktop AI agent and companion with
// smrti long-term memory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/app"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel/phone"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/gateway"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/tui"
	"github.com/cyqlelabs/factor/internal/version"
	"github.com/cyqlelabs/factor/internal/wizard"
)

const usage = `factor — desktop AI agent with smrti memory

Usage:
  factor                 interactive chat
  factor -m "message"    one-shot message
  factor -s NAME         use a named session (default "main")
  factor gateway         run the daemon (channels, cron, heartbeat)
  factor init            interactive setup wizard (provider, memory, channels)
  factor status          show daemon, provider, and memory status
  factor version         print version

Flags:
  -c PATH      config file (default ~/.factor/config.json)
  -y           init: skip the wizard and accept the defaults
  --no-install init: never install smrti, desktop helpers, or a browser
`

func main() {
	fs := flag.NewFlagSet("factor", flag.ExitOnError)
	configPath := fs.String("c", "", "config file path")
	message := fs.String("m", "", "one-shot message")
	sessionName := fs.String("s", "main", "session name")
	yes := fs.Bool("y", false, "init: skip the wizard, accept defaults")
	noInstall := fs.Bool("no-install", false, "init: never install anything")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "version":
		fmt.Printf("factor %s (%s, built %s)\n", version.Version, version.GitCommit, version.BuildTime)
	case "init":
		err = runInit(*configPath, *yes, *noInstall)
	case "status":
		err = runStatus(*configPath)
	case "gateway":
		err = gateway.Run(*configPath)
	case "":
		err = runChat(*configPath, *sessionName, *message)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "factor: %v\n", err)
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runInit(configPath string, nonInteractive, noInstall bool) error {
	ctx, cancel := signalContext()
	defer cancel()

	err := wizard.Run(ctx, configPath, wizard.Options{
		Version:        version.Version,
		NonInteractive: nonInteractive,
		NoInstall:      noInstall,
	})
	if errors.Is(err, wizard.ErrAborted) {
		return nil // the wizard already said so; not a failure
	}
	return err
}

func runStatus(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	fmt.Printf("factor %s\n", version.Version)
	fmt.Printf("config:    %s\n", cfg.Path())
	fmt.Printf("workspace: %s\n", cfg.Agent.Workspace)
	fmt.Printf("provider:  %s %s\n", cfg.Provider.Type, cfg.Provider.Model)

	if pid, alive := gateway.ReadPidFile(); alive {
		fmt.Printf("gateway:   running (pid %d)\n", pid)
	} else {
		fmt.Printf("gateway:   not running\n")
	}

	if path, ok := memory.FindSmrti(cfg.Memory.Command, config.Home()); ok {
		fmt.Printf("smrti:     %s\n", path)
	} else {
		fmt.Printf("smrti:     not installed (it will be installed on demand)\n")
	}

	if raw, configured := cfg.Channels["phone"]; configured {
		phoneCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		status := phone.Describe(phoneCtx, raw, config.Home())
		cancel()
		fmt.Printf("phone:     %s\n", status.Line())
		// Only offer the install when there is nothing to run and nothing
		// running: saying "not installed" under "healthy" is a contradiction
		// the reader has to go and disprove.
		if status.Python == "" && !status.Healthy && status.Enabled {
			fmt.Printf("           voice shell not installed (it will be installed on demand)\n")
		}
	}

	env := desktop.DefaultEnv()
	if cfg.Desktop.Register(desktop.HasDisplay(env)) {
		ctl := desktop.NewController(env)
		line := "desktop:   " + ctl.Backend()
		if missing := desktop.MissingHelpers(env, ctl); len(missing) > 0 {
			names := make([]string, 0, len(missing))
			for _, h := range missing {
				names = append(names, h.Bin)
			}
			line += " — missing " + strings.Join(names, ", ")
		} else {
			line += " — all helpers present"
		}
		fmt.Println(line)
	} else {
		fmt.Println("desktop:   off (no graphical session)")
	}

	client := memory.NewClient(cfg.Memory.BaseURL(), cfg.Memory.APIKey, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if status, err := client.Status(ctx); err != nil {
		fmt.Printf("memory:    unreachable at %s (%v)\n", cfg.Memory.BaseURL(), err)
	} else {
		fmt.Printf("memory:    healthy at %s — %v atoms\n", cfg.Memory.BaseURL(), status["total_atoms"])
	}
	return nil
}

func runChat(configPath, sessionName, message string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()

	baseName, sessionKey := sessionName, "cli:"+sessionName

	if message != "" {
		return runOneShot(ctx, a, message, sessionKey)
	}

	con := tui.NewChat(os.Stdin, os.Stdout)
	con.Start()
	defer con.Close()
	if con.Interactive() {
		// Logging goes through the console, or a warning lands mid-prompt.
		log.SetOutput(con.LogWriter())
		defer log.SetOutput(os.Stderr)
	}

	ui := newChatUI(con)
	ui.bar = func() tui.Bar { return chatBar(sessionName, cfg.Provider.Model, a.Memory) }
	a.Loop.OnActivity(ui.activity)
	ui.refreshBar()
	go ui.watchBar(ctx, 2*time.Second)

	con.Printf("factor %s — %s | /quit to exit, /new for a fresh session",
		version.Version, cfg.Provider.Model)

	// Bus-driven REPL: replies AND proactive messages (finished background
	// jobs, steered turns) print as they arrive, above the live prompt.
	go a.Loop.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case out := <-a.Bus.Outbound():
				if out.Channel == "cli" {
					ui.reply(out.Content)
				}
			}
		}
	}()

	for {
		line, err := con.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, tui.ErrInterrupted) {
				return nil
			}
			return err
		}
		switch line = strings.TrimSpace(line); line {
		case "":
			continue
		case "/quit", "/exit":
			return nil
		case "/new":
			sessionName = fmt.Sprintf("%s-%d", baseName, time.Now().Unix())
			sessionKey = "cli:" + sessionName
			con.Printf("(started a fresh session)")
			ui.refreshBar()
			continue
		}
		chatID := strings.TrimPrefix(sessionKey, "cli:")
		ui.begin(sessionKey)
		if !a.Bus.PublishInbound(bus.InboundMessage{Channel: "cli", ChatID: chatID, Content: line, Time: time.Now()}) {
			ui.abort()
			con.Printf("(too busy to take that message — try again in a moment)")
		}
	}
}

// runOneShot answers a single -m message, pulsing while it works so a slow
// turn does not look like a hung terminal.
func runOneShot(ctx context.Context, a *app.App, message, sessionKey string) error {
	con := tui.NewStatus(os.Stdout)
	defer con.Close()
	spin := tui.NewSpinner(con)
	a.Loop.OnActivity(func(act agent.Activity) {
		if act.SessionKey != sessionKey {
			return
		}
		if act.Phase == agent.PhaseDone {
			spin.Stop()
			return
		}
		spin.Set(string(act.Phase), act.Detail)
	})

	spin.Start()
	reply, err := a.Loop.ProcessDirect(ctx, message, sessionKey)
	spin.Stop()
	if err != nil {
		return err
	}
	fmt.Println(reply)
	a.Loop.WaitBackground(2 * time.Minute) // let memory writes land (bounded by StoreExchange itself)
	return nil
}

// chatUI joins the console, the pulse, and the loop's activity events into
// one view: a message you send stays on screen with a live pulse under it
// until its reply lands, and the prompt says so while you wait.
type chatUI struct {
	con  *tui.Console
	spin *tui.Spinner
	bar  func() tui.Bar

	mu      sync.Mutex
	live    string       // session key of the turn being waited on, "" when idle
	summary *tui.Summary // the finished turn's numbers, consumed by its reply
}

func newChatUI(con *tui.Console) *chatUI {
	return &chatUI{con: con, spin: tui.NewSpinner(con)}
}

// begin marks a turn as in flight the moment its message is sent, before the
// loop has picked it up.
func (u *chatUI) begin(sessionKey string) { u.start(sessionKey, false) }

// adopt does the same for a turn the user did not start, but only when
// nothing else is already being waited on.
func (u *chatUI) adopt(sessionKey string) { u.start(sessionKey, true) }

func (u *chatUI) start(sessionKey string, onlyWhenIdle bool) {
	u.mu.Lock()
	idle := u.live == ""
	if !idle && onlyWhenIdle {
		u.mu.Unlock()
		return
	}
	u.live = sessionKey
	u.mu.Unlock()
	if idle {
		u.spin.Start()
		u.con.PromptSteering()
		u.refreshBar()
	}
}

// refreshBar redraws the status bar: the session name and the memory
// engine's health both change under it.
func (u *chatUI) refreshBar() {
	if u.bar != nil {
		u.con.SetBar(u.bar())
	}
}

// watchBar keeps the bar honest while the prompt sits idle: memory health
// flips asynchronously (sidecar warm-up, outages, recovery) and turn
// boundaries are the only other repaints. SetBar drops identical repaints,
// so a tick that changes nothing draws nothing.
func (u *chatUI) watchBar(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.refreshBar()
		}
	}
}

// chatBar is what the bar says: where you are, what is answering, and the
// keys that are not guessable.
func chatBar(session, model string, mem memory.Engine) tui.Bar {
	bar := tui.Bar{
		Session: session,
		Model:   model,
		Hints:   []string{"alt+⏎ newline", "↑ history", "/quit"},
	}
	if mem != nil && mem.Enabled() {
		if bar.Memory = "memory ✓"; !mem.Healthy() {
			bar.Memory = "memory ✗"
		}
	}
	return bar
}

// activity mirrors the loop's phases onto the pulse. Turns nobody asked for
// (a finished background job re-entering the session) are adopted too, so
// unprompted work is visible rather than mysterious.
func (u *chatUI) activity(act agent.Activity) {
	if !strings.HasPrefix(act.SessionKey, "cli:") {
		return
	}
	if act.Phase != agent.PhaseDone {
		u.adopt(act.SessionKey)
	}
	u.mu.Lock()
	mine := u.live == act.SessionKey
	u.mu.Unlock()
	if !mine {
		return
	}
	if act.Phase == agent.PhaseDone {
		u.end()
		return
	}
	u.spin.Set(string(act.Phase), act.Detail)
}

// end stops the pulse and keeps its summary for the reply that follows.
func (u *chatUI) end() {
	u.mu.Lock()
	if u.live == "" {
		u.mu.Unlock()
		return
	}
	u.live = ""
	u.mu.Unlock()

	sum := u.spin.Stop()
	u.mu.Lock()
	u.summary = &sum
	u.mu.Unlock()
	u.con.PromptIdle()
	u.refreshBar()
}

// abort ends a turn that never started, leaving no summary behind.
func (u *chatUI) abort() {
	u.end()
	u.mu.Lock()
	u.summary = nil
	u.mu.Unlock()
}

func (u *chatUI) reply(content string) {
	u.end()
	u.mu.Lock()
	note := ""
	if u.summary != nil {
		note = u.summary.Note()
		u.summary = nil
	}
	u.mu.Unlock()
	u.con.Reply(content, note)
}
