// Command factor is a fast, reliable desktop AI agent and companion with
// smrti long-term memory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cyqlelabs/factor/internal/agent"
	"github.com/cyqlelabs/factor/internal/app"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/channel"
	"github.com/cyqlelabs/factor/internal/channel/phone"
	"github.com/cyqlelabs/factor/internal/channel/voice"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/gateway"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/proxy"
	"github.com/cyqlelabs/factor/internal/tray"
	"github.com/cyqlelabs/factor/internal/tui"
	"github.com/cyqlelabs/factor/internal/upgrade"
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
  factor talk            push-to-talk: arm the PC voice channel's microphone
  factor status          show daemon, provider, memory, channel, and desktop status
  factor upgrade         install the newest release
  factor version         print version

Flags:
  -c PATH      config file (default ~/.factor/config.json)
  -p ADDR      route HTTP through a proxy (e.g. -p 127.0.0.1:8080) so every
               provider call can be inspected; loopback and the browser are
               left alone. mitmproxy's CA is trusted when installed, or name
               another with --proxy-ca.
  -d           gateway: run in the background (logs to ~/.factor/gateway.log)
  -y           init: skip the wizard and accept the defaults
  --no-install init: never install smrti, desktop helpers, or a browser
  --check      upgrade: report the newest release without installing it
`

func main() {
	fs := flag.NewFlagSet("factor", flag.ExitOnError)
	configPath := fs.String("c", "", "config file path")
	proxyAddr := fs.String("p", "", "route HTTP through this proxy")
	proxyCA := fs.String("proxy-ca", "", "certificate authority the proxy signs with")
	message := fs.String("m", "", "one-shot message")
	sessionName := fs.String("s", "main", "session name")
	daemon := fs.Bool("d", false, "gateway: run in the background")
	yes := fs.Bool("y", false, "init: skip the wizard, accept defaults")
	noInstall := fs.Bool("no-install", false, "init: never install anything")
	checkOnly := fs.Bool("check", false, "upgrade: report the newest release without installing it")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	_ = fs.Parse(args)

	// Before anything builds an HTTP client or spawns a sidecar: the proxy is
	// read from the environment once, and the children inherit it.
	if *proxyAddr != "" {
		line, perr := proxy.Use(*proxyAddr, *proxyCA)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "factor: %v\n", perr)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "factor: "+line)
	}

	var err error
	switch cmd {
	case "version":
		fmt.Printf("factor %s (%s, built %s)\n", version.Version, version.GitCommit, version.BuildTime)
	case "init":
		err = runInit(*configPath, *yes, *noInstall)
	case "status":
		err = runStatus(*configPath)
	case "upgrade":
		err = runUpgrade(*configPath, *checkOnly)
	case "gateway":
		err = runGateway(*configPath, *daemon)
	case "talk":
		err = runTalk(*configPath)
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

// runGateway runs the daemon here, or hands it to a detached background
// process when -d asks for that — in which case success means the child
// confirmed it is serving, not that this process ran it.
func runGateway(configPath string, detach bool) error {
	if detach {
		pid, err := daemonize(configPath)
		if err != nil {
			return err
		}
		fmt.Printf("gateway running in the background (pid %d) — logs at %s\n", pid, gateway.LogPath())
		return nil
	}
	// The tray owns the main goroutine — its event loop must run there on
	// some platforms — so the gateway serves from a goroutine and its return
	// takes the icon down with it. Where no tray can be shown, trayRun comes
	// straight back and this reduces to waiting on the gateway.
	done := make(chan error, 1)
	go func() {
		err := gatewayRun(configPath)
		trayQuit()
		done <- err // sent last, so a returned runGateway has nothing in flight
	}()
	trayRun(version.Version, gateway.StatusLines, gateway.RequestStop)
	return <-done
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

// Seams: an upgrade test must not depend on what GitHub is publishing today,
// nor on what this machine happens to be running in docker.
var (
	latestRelease  = upgrade.Latest
	applyRelease   = upgrade.Apply
	restartGateway = gateway.SignalRestart
	daemonize      = gateway.Daemonize
	gatewayRun     = gateway.Run
	trayRun        = tray.Run
	trayQuit       = tray.Quit
	updateEngine   = func(ctx context.Context, configPath string, checkOnly bool) error {
		cfg, err := config.Load(configPath)
		if err != nil || cfg.Memory.Mode == "off" {
			return err
		}
		return upgrade.NewSmrti(cfg.Memory, gatewayMemoryIdle(cfg)).Update(ctx, checkOnly,
			func(format string, args ...any) { fmt.Printf(format+"\n", args...) })
	}
)

func runUpgrade(configPath string, checkOnly bool) error {
	ctx, cancel := signalContext()
	defer cancel()

	// The memory engine first: it is swapped in place, so it neither needs nor
	// survives a Factor restart, and doing it before the binary keeps the
	// restart below as the last thing that happens. Its failures are reported
	// rather than fatal — a Factor that cannot reach the image registry, or
	// whose engine is a pip install, still upgrades itself.
	if err := updateEngine(ctx, configPath, checkOnly); err != nil && !errors.Is(err, upgrade.ErrNotContainerised) {
		fmt.Fprintf(os.Stderr, "smrti: %v\n", err)
	}

	rel, err := latestRelease(ctx)
	if err != nil {
		return err
	}
	if !upgrade.Newer(version.Version, rel.Version) {
		fmt.Printf("factor %s is the newest release.\n", version.Version)
		return nil
	}
	if checkOnly {
		fmt.Printf("factor %s is available — this one is %s.\n%s\n", rel.Version, version.Version, rel.Notes)
		return nil
	}

	path, err := applyRelease(ctx, rel, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})
	if err != nil {
		return err
	}
	fmt.Printf("installed factor %s at %s\n", rel.Version, path)
	// The daemon is a separate process that already loaded its code, so the
	// upgrade is only half done until it reloads. It restarts itself once the
	// conversation it is in the middle of has been answered.
	if pid, alive := gateway.ReadPidFile(); alive {
		if err := restartGateway(pid); err != nil {
			fmt.Printf("the gateway is still running %s (pid %d) — restart it to pick this up (%v)\n",
				version.Version, pid, err)
		} else {
			fmt.Printf("the running gateway (pid %d) will restart into %s once it is idle\n", pid, rel.Version)
		}
	}
	return nil
}

// gatewayMemoryIdle reports whether the daemon's memory engine is quiet. Only
// the gateway knows what it has in flight, so a terminal asks it over the
// health endpoint; with no daemon running, nothing is using the engine and the
// swap can go ahead.
func gatewayMemoryIdle(cfg *config.Config) func() bool {
	if _, alive := gateway.ReadPidFile(); !alive {
		return nil
	}
	url := fmt.Sprintf("http://%s:%d/health", cfg.Gateway.Host, cfg.Gateway.Port)
	client := &http.Client{Timeout: 3 * time.Second}
	return func() bool {
		resp, err := client.Get(url)
		if err != nil {
			return false // unconfirmed is not idle
		}
		defer resp.Body.Close()
		var health struct {
			MemoryIdle *bool `json:"memory_idle"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&health); err != nil {
			return false
		}
		// A daemon too old to report it has no opinion, and waiting for an
		// answer it will never give would stall the first upgrade that brings
		// the field with it.
		return health.MemoryIdle == nil || *health.MemoryIdle
	}
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

	// The engine is probed before the binary is reported: a smrti running in
	// Docker or on another box has no local file to find, and printing
	// "not installed" above a healthy memory is a contradiction the reader
	// has to go and disprove.
	memClient := memory.NewClient(cfg.Memory.BaseURL(), cfg.Memory.APIKey, "")
	memCtx, memCancel := context.WithTimeout(context.Background(), 3*time.Second)
	memStatus, memErr := memClient.Status(memCtx)
	memCancel()

	if path, ok := memory.FindSmrti(cfg.Memory.Command, config.Home()); ok {
		fmt.Printf("smrti:     %s\n", path)
	} else if memErr != nil && cfg.Memory.Mode == "sidecar" {
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

	if raw, configured := cfg.Channels["voice"]; configured {
		voiceCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		voiceStatus := voice.Describe(voiceCtx, raw, voice.DefaultEnv())
		cancel()
		fmt.Printf("voice:     %s\n", voiceStatus.Line())
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

	if memErr != nil {
		fmt.Printf("memory:    unreachable at %s (%v)\n", cfg.Memory.BaseURL(), memErr)
	} else {
		fmt.Printf("memory:    healthy at %s — %v atoms\n", cfg.Memory.BaseURL(), memStatus["total_atoms"])
	}
	return nil
}

// runTalk arms push-to-talk on the process running the voice channel — the
// daemon, or a chat session in another terminal.
func runTalk(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	raw, configured := cfg.Channels["voice"]
	if !configured {
		return errors.New("PC voice is not configured — run `factor init` to set it up")
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := voice.Talk(ctx, raw); err != nil {
		return err
	}
	fmt.Println("listening — speak now")
	return nil
}

// startVoiceChannel brings the PC voice channel up inside a chat session, so
// the microphone works without the gateway. It reports whether it started and
// hands back the push-to-talk trigger for the /talk command; the returned
// stop is always safe to call.
func startVoiceChannel(ctx context.Context, a *app.App, cfg *config.Config, sessionName string) (stop func(), talk func(), meter func() voice.Meter, started bool) {
	raw, configured := cfg.Channels["voice"]
	if !configured {
		return func() {}, nil, nil, false
	}
	channels := channel.Build(map[string]json.RawMessage{"voice": raw}, a.Bus)
	if len(channels) == 0 {
		return func() {}, nil, nil, false
	}
	ch := channels[0]
	if runner, ok := ch.(channel.TurnRunner); ok {
		runner.BindTurnRunner(a.Loop.ProcessDirectNotice)
	}
	if addresser, ok := ch.(channel.Addresser); ok {
		// A written reply lands in this terminal: the drain below prints
		// every outbound cli message.
		addresser.BindLastExternal(func() (string, string, bool) { return "cli", sessionName, true })
	}
	if provider, ok := ch.(channel.Toolset); ok {
		a.Registry.Register(provider.Toolset()...)
	}
	if err := ch.Start(ctx); err != nil {
		log.Printf("voice channel failed to start: %v", err)
		return func() {}, nil, nil, false
	}
	if talker, ok := ch.(interface{ ArmPTT() }); ok {
		talk = talker.ArmPTT
	}
	if metered, ok := ch.(interface{ Meter() voice.Meter }); ok {
		meter = metered.Meter
	}
	return func() { _ = ch.Stop() }, talk, meter, true
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

	// The PC voice channel listens here too, not only under the gateway —
	// registered before the loop runs so its tool exists from the first turn,
	// and before the bar so its meter shows from the first repaint.
	stopVoice, talk, meter, voiceOn := startVoiceChannel(ctx, a, cfg, baseName)
	defer stopVoice()

	ui := newChatUI(con)
	ui.bar = func() tui.Bar {
		return chatBar(sessionName, cfg.Provider.Model, a.Cost.SessionLine(sessionKey), a.Memory, meter)
	}
	a.Loop.OnActivity(ui.activity)
	ui.refreshBar()
	// A live microphone meter needs a livelier bar than the memory dot does.
	barEvery := 2 * time.Second
	if meter != nil {
		barEvery = 300 * time.Millisecond
	}
	go ui.watchBar(ctx, barEvery)

	con.Printf("factor %s — %s | /quit to exit, /new for a fresh session",
		version.Version, cfg.Provider.Model)
	if voiceOn {
		con.Printf("voice: listening on the microphone (/talk for push-to-talk)")
	}

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
		case "/talk":
			if talk == nil {
				con.Printf("(PC voice is not running — set it up with `factor init`)")
				continue
			}
			talk()
			con.Printf("(listening — speak now)")
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
		if act.Phase == agent.PhaseNotice {
			con.Printf("%s", act.Detail)
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
func chatBar(session, model, spend string, mem memory.Engine, meter func() voice.Meter) tui.Bar {
	bar := tui.Bar{
		Session: session,
		Model:   model,
		Cost:    spend,
		Hints:   []string{"alt+⏎ newline", "↑ history", "/quit"},
	}
	if mem != nil && mem.Enabled() {
		if bar.Memory = "memory ✓"; !mem.Healthy() {
			bar.Memory = "memory ✗"
		}
	}
	if meter != nil {
		bar.Voice, bar.VoiceTone = voiceBarSegment(meter())
		bar.Hints = append([]string{"/talk"}, bar.Hints...)
	}
	return bar
}

// voiceBarSegment renders the channel's ears and mouth for the bar: a live
// VU meter for the microphone, a note while the agent speaks.
func voiceBarSegment(m voice.Meter) (text, tone string) {
	switch {
	case !m.Ready:
		return "mic …", ""
	case m.Silent:
		return "mic ✗", "warn"
	}
	speech := "·"
	if m.Speaking {
		speech = "♪"
	}
	text = "mic " + vuMeter(m.Level, m.Floor) + " " + speech
	switch {
	case m.Speaking:
		return text, "speak"
	case m.Floor > 0 && m.Level >= m.Floor*2:
		return text, "hear"
	}
	return text, ""
}

// vuMeter draws three cells that light up as the microphone level climbs over
// the noise floor.
func vuMeter(level, floor float64) string {
	if floor <= 0 {
		return "▁▁▁"
	}
	ratio := level / floor
	var b strings.Builder
	for _, cell := range []struct {
		at  float64
		lit string
	}{{1.5, "▂"}, {3, "▄"}, {6, "▆"}} {
		if ratio >= cell.at {
			b.WriteString(cell.lit)
		} else {
			b.WriteString("▁")
		}
	}
	return b.String()
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
	// A note about what comes next belongs on screen, not on the activity
	// line that the next phase overwrites — the pulse keeps running under it.
	if act.Phase == agent.PhaseNotice {
		u.con.Printf("%s", act.Detail)
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
