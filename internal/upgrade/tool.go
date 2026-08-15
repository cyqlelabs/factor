package upgrade

import (
	"context"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool lets the agent answer "are you up to date?" and act on the answer,
// which is how the upgrade reaches a box the user only ever talks to over
// Telegram.
type Tool struct {
	Current string     // the running build, from internal/version
	Restart *Restarter // how this process reloads, when it can (the gateway)
}

func (t *Tool) Name() string { return "upgrade" }

func (t *Tool) Description() string {
	return "Check whether a newer Factor release exists and install it. action=check reports what is available; action=install downloads that release, verifies its published checksum, and replaces this binary. Read the install result before describing it: it says whether the new version is loading now or waits for the next start."
}

func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []any{"check", "install"},
				"description": "check (default) reports the newest release; install replaces the binary",
			},
		},
	}
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	rel, err := Latest(ctx)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if !Newer(t.Current, rel.Version) {
		return tools.Textf("factor %s is the newest release.", t.Current)
	}
	if tools.StringArg(args, "action") != "install" {
		return tools.Textf("factor %s is available (this one is %s): %s", rel.Version, t.Current, rel.Notes)
	}
	path, err := Apply(ctx, rel, nil)
	if err != nil {
		return tools.Errorf("upgrading to factor %s: %v", rel.Version, err)
	}
	// The restart waits for this turn to be answered, so the reply below is
	// the user's warning that Factor is about to go quiet for a moment.
	if t.Restart.Request("installed factor " + rel.Version) {
		return tools.Textf("Installed factor %s at %s, replacing %s. Restarting into it as soon as this answer reaches you — say goodbye briefly; you will be back in a few seconds.",
			rel.Version, path, t.Current)
	}
	return tools.Textf("Installed factor %s at %s, replacing %s. It takes effect on the next start.",
		rel.Version, path, t.Current)
}
