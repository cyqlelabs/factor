// Command factor is a fast, reliable desktop AI agent and companion with
// smrti long-term memory.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cyqlelabs/factor/internal/app"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/gateway"
	"github.com/cyqlelabs/factor/internal/memory"
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
  --no-install init: never install smrti or desktop helpers
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

	sessionKey := "cli:" + sessionName

	if message != "" {
		reply, err := a.Loop.ProcessDirect(ctx, message, sessionKey)
		if err != nil {
			return err
		}
		fmt.Println(reply)
		a.Loop.WaitBackground(2 * time.Minute) // let memory writes land (bounded by StoreExchange itself)
		return nil
	}

	fmt.Printf("factor %s — %s | session %s | /quit to exit, /new for a fresh session\n",
		version.Version, cfg.Provider.Model, sessionName)

	// Bus-driven REPL: replies AND proactive messages (finished background
	// jobs, steered turns) print as they arrive.
	go a.Loop.Run(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case out := <-a.Bus.Outbound():
				if out.Channel == "cli" {
					fmt.Printf("\rfactor> %s\n\nyou> ", out.Content)
				}
			}
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("you> ")
		if !scanner.Scan() {
			fmt.Println()
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "":
			continue
		case "/quit", "/exit":
			return nil
		case "/new":
			sessionKey = fmt.Sprintf("cli:%s-%d", sessionName, time.Now().Unix())
			fmt.Println("(started a fresh session)")
			continue
		}
		chatID := strings.TrimPrefix(sessionKey, "cli:")
		a.Bus.PublishInbound(bus.InboundMessage{Channel: "cli", ChatID: chatID, Content: line, Time: time.Now()})
	}
}
