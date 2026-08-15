package upgrade

import (
	"context"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool lets the agent answer "are you up to date?" and act on the answer,
// which is how the upgrade reaches a box the user only ever talks to over
// Telegram.
type Tool struct {
	Current string // the running build, from internal/version
}

func (t *Tool) Name() string { return "upgrade" }

func (t *Tool) Description() string {
	return "Check whether a newer Factor release exists and install it. action=check reports what is available; action=install downloads that release, verifies its published checksum, and replaces this binary. An install takes effect when Factor next starts — say so rather than claiming the new version is already running."
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
	return tools.Textf("Installed factor %s at %s, replacing %s. It takes effect on the next start.",
		rel.Version, path, t.Current)
}
