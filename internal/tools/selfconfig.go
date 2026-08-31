package tools

import (
	"context"
	"encoding/json"

	"github.com/cyqlelabs/factor/internal/config"
)

// NewConfigTools lets the agent inspect and modify its own configuration.
// Both tools operate on the config FILE (the live in-memory config is
// immutable while running): reads are secret-redacted, writes are
// schema-validated and persisted atomically. A running gateway watches the
// file and reloads itself to apply what changed; a plain chat session picks
// the change up on its next start.
func NewConfigTools(cfg *config.Config) []Tool {
	path := cfg.Path()
	return []Tool{
		&configGetTool{path: path},
		&configSetTool{path: path},
	}
}

type configGetTool struct {
	ReadOnly
	path string
}

func (t *configGetTool) Name() string { return "config_get" }
func (t *configGetTool) Description() string {
	return "Read Factor's configuration file (secrets are redacted). Optional dotted key, e.g. 'provider.model' or 'tools.disabled'; omit for the full config."
}
func (t *configGetTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"key": map[string]any{"type": "string", "description": "Dotted path such as provider.model; omit to return the whole config"}},
	}
}
func (t *configGetTool) Execute(_ context.Context, args map[string]any) *Result {
	cfg, err := config.ReadFile(t.path)
	if err != nil {
		return Errorf("%v", err)
	}
	val, err := cfg.Get(StringArg(args, "key"))
	if err != nil {
		return Errorf("%v", err)
	}
	data, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return Errorf("%v", err)
	}
	return Text(string(data))
}

type configSetTool struct {
	path string
}

func (t *configSetTool) Name() string { return "config_set" }
func (t *configSetTool) Description() string {
	return "Set one configuration value by dotted key (e.g. key='heartbeat.interval_minutes', value=15) and persist it to the config file. Under the gateway it applies within seconds — the daemon reloads itself between turns; a plain chat session applies it on the next start. Confirm with the user before changing provider credentials."
}
func (t *configSetTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key":   map[string]any{"type": "string", "description": "Dotted path such as heartbeat.interval_minutes; read it with config_get first if unsure it exists"},
			"value": map[string]any{"description": "New value (any JSON type)"},
		},
		"required": []any{"key", "value"},
	}
}
func (t *configSetTool) Execute(_ context.Context, args map[string]any) *Result {
	key := StringArg(args, "key")
	err := config.Update(t.path, func(cfg *config.Config) error {
		return cfg.Set(key, args["value"])
	})
	if err != nil {
		return Errorf("%v", err)
	}
	return Textf("Set %s and saved the config file. A running gateway applies it within seconds by reloading itself; "+
		"a plain chat session applies it on its next start.", key)
}
