package upgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool lets the agent answer "are you up to date?" and act on the answer,
// which is how the upgrade reaches a box the user only ever talks to over
// Telegram. Factor and smrti are both covered: a Factor that upgrades itself
// and leaves its memory engine two versions behind is only half current.
type Tool struct {
	Current string     // the running build, from internal/version
	Restart *Restarter // how this process reloads, when it can (the gateway)
	Smrti   *Smrti     // the memory engine's upgrader; nil when memory is off
}

func (t *Tool) Name() string { return "upgrade" }

func (t *Tool) Description() string {
	return "Check whether newer releases of Factor or smrti (the memory engine) exist and install them. action=check reports what is available; action=install downloads it — Factor's binary is verified against its published checksum, smrti's container is replaced with the newly published image once the memory graph is idle. component limits the work to one of them (default both). Read the install result before describing it: it says whether the new Factor is loading now or waits for the next start."
}

func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []any{"check", "install"},
				"description": "check (default) reports what is available; install applies it",
			},
			"component": map[string]any{
				"type":        "string",
				"enum":        []any{"all", "factor", "smrti"},
				"description": "what to act on (default all)",
			},
		},
	}
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	install := tools.StringArg(args, "action") == "install"
	// Anything but one of the two halves means both of them: a component the
	// model invented must not silently answer about nothing at all.
	component := tools.StringArg(args, "component")
	if component != "factor" && component != "smrti" {
		component = "all"
	}

	var lines []string
	failed := false
	if component == "all" || component == "smrti" {
		if line, ok := t.engine(ctx, install, component == "smrti"); line != "" {
			lines = append(lines, line)
			failed = failed || !ok
		}
	}
	if component == "all" || component == "factor" {
		line, ok := t.factor(ctx, install)
		lines = append(lines, line)
		failed = failed || !ok
	}

	if failed {
		return tools.Errorf("%s", strings.Join(lines, "\n"))
	}
	return tools.Text(strings.Join(lines, "\n"))
}

// factor reports on — and optionally installs — the newest Factor release.
func (t *Tool) factor(ctx context.Context, install bool) (string, bool) {
	rel, err := Latest(ctx)
	if err != nil {
		return err.Error(), false
	}
	if !Newer(t.Current, rel.Version) {
		return fmt.Sprintf("factor %s is the newest release.", t.Current), true
	}
	if !install {
		return fmt.Sprintf("factor %s is available (this one is %s): %s", rel.Version, t.Current, rel.Notes), true
	}
	path, err := Apply(ctx, rel, nil)
	if err != nil {
		return fmt.Sprintf("upgrading to factor %s: %v", rel.Version, err), false
	}
	// The restart waits for this turn to be answered, so the reply below is
	// the user's warning that Factor is about to go quiet for a moment — and
	// the chat it is being sent to is where the new process reports back in.
	tc := tools.ToolContextFrom(ctx)
	if t.Restart.Request("installed factor "+rel.Version, Target{Channel: tc.Channel, ChatID: tc.ChatID}) {
		return fmt.Sprintf("Installed factor %s at %s, replacing %s. Restarting into it as soon as this answer reaches you — say goodbye briefly; you will be back in a few seconds.",
			rel.Version, path, t.Current), true
	}
	return fmt.Sprintf("Installed factor %s at %s, replacing %s. It takes effect on the next start.",
		rel.Version, path, t.Current), true
}

// engine reports on — and optionally installs — the newest smrti image. An
// empty line means there is nothing to say: memory is off, or the engine is
// not a container and the caller did not ask about it specifically.
func (t *Tool) engine(ctx context.Context, install, asked bool) (string, bool) {
	if t.Smrti == nil {
		if asked {
			return "memory is off, so there is no smrti to upgrade.", true
		}
		return "", true
	}
	rel, err := t.Smrti.Check(ctx)
	if err != nil {
		if errors.Is(err, ErrNotContainerised) && !asked {
			return "", true // there is no image half here; only say so if asked
		}
		return err.Error(), false
	}
	if !rel.Newer() {
		return fmt.Sprintf("smrti %s is the newest published image.", rel.Running), true
	}
	if !install {
		return fmt.Sprintf("smrti %s is available (the engine runs %s).", rel.Version, rel.Running), true
	}
	if err := t.Smrti.Apply(ctx, rel, nil); err != nil {
		return fmt.Sprintf("upgrading smrti to %s: %v", rel.Version, err), false
	}
	return fmt.Sprintf("Upgraded smrti from %s to %s; the engine is answering again and its memory is untouched.",
		rel.Running, rel.Version), true
}
