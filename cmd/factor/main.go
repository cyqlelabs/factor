// Command factor is a fast, reliable desktop AI agent and companion with
// smrti long-term memory.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cyqlelabs/factor/internal/app"
	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/gateway"
	"github.com/cyqlelabs/factor/internal/memory"
	"github.com/cyqlelabs/factor/internal/version"
)

const usage = `factor — desktop AI agent with smrti memory

Usage:
  factor                 interactive chat
  factor -m "message"    one-shot message
  factor -s NAME         use a named session (default "main")
  factor gateway         run the daemon (channels, cron, heartbeat)
  factor init            create config + workspace, check dependencies
  factor status          show daemon, provider, and memory status
  factor version         print version

Flags:
  -c PATH   config file (default ~/.factor/config.json)
`

func main() {
	fs := flag.NewFlagSet("factor", flag.ExitOnError)
	configPath := fs.String("c", "", "config file path")
	message := fs.String("m", "", "one-shot message")
	sessionName := fs.String("s", "main", "session name")
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
		err = runInit(*configPath)
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

func runInit(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(cfg.Path()); os.IsNotExist(statErr) {
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("Created config: %s\n", cfg.Path())
	} else {
		fmt.Printf("Config exists: %s\n", cfg.Path())
	}
	if err := config.EnsureWorkspace(cfg.Agent.Workspace); err != nil {
		return err
	}
	fmt.Printf("Workspace ready: %s\n", cfg.Agent.Workspace)

	if cfg.Provider.APIKey == "" {
		fmt.Println("\n⚠ No provider API key set. Edit the config or export FACTOR_PROVIDER_API_KEY.")
	}
	if _, err := exec.LookPath(cfg.Memory.Command); err != nil {
		fmt.Printf("\n⚠ smrti not found in PATH — Factor's memory engine.\n  Install it with: pip install smrti\n")
	} else {
		fmt.Println("smrti found — memory engine ready.")
	}
	fmt.Println("\nRun `factor` to chat or `factor gateway` for the daemon.")
	return nil
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
		switch {
		case line == "":
			continue
		case line == "/quit" || line == "/exit":
			return nil
		case line == "/new":
			sessionKey = fmt.Sprintf("cli:%s-%d", sessionName, time.Now().Unix())
			fmt.Println("(started a fresh session)")
			continue
		}
		chatID := strings.TrimPrefix(sessionKey, "cli:")
		a.Bus.PublishInbound(bus.InboundMessage{Channel: "cli", ChatID: chatID, Content: line, Time: time.Now()})
	}
}
