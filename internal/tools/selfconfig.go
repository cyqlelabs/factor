package tools

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/cyqlelabs/factor/internal/config"
)

// NewConfigTools lets the agent inspect and modify its own configuration.
// Reads are secret-redacted; writes are schema-validated and persisted.
func NewConfigTools(cfg *config.Config) []Tool {
	mu := &sync.Mutex{}
	return []Tool{
		&configGetTool{cfg: cfg, mu: mu},
		&configSetTool{cfg: cfg, mu: mu},
	}
}

type configGetTool struct {
	cfg *config.Config
	mu  *sync.Mutex
}

func (t *configGetTool) Name() string { return "config_get" }
func (t *configGetTool) Description() string {
	return "Read Factor's own configuration (secrets are redacted). Optional dotted key, e.g. 'provider.model' or 'tools.disabled'; omit for the full config."
}
func (t *configGetTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"key": map[string]any{"type": "string"}},
	}
}
func (t *configGetTool) Execute(_ context.Context, args map[string]any) *Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	val, err := t.cfg.Get(StringArg(args, "key"))
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
	cfg *config.Config
	mu  *sync.Mutex
}

func (t *configSetTool) Name() string { return "config_set" }
func (t *configSetTool) Description() string {
	return "Set one configuration value by dotted key (e.g. key='heartbeat.interval_minutes', value=15) and persist it. Prompt/tool settings apply on the next turn; provider and memory changes need a restart. Confirm with the user before changing provider credentials."
}
func (t *configSetTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key":   map[string]any{"type": "string"},
			"value": map[string]any{"description": "New value (any JSON type)"},
		},
		"required": []any{"key", "value"},
	}
}
func (t *configSetTool) Execute(_ context.Context, args map[string]any) *Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := StringArg(args, "key")
	if err := t.cfg.Set(key, args["value"]); err != nil {
		return Errorf("%v", err)
	}
	if err := t.cfg.Save(); err != nil {
		return Errorf("set applied in memory but save failed: %v", err)
	}
	return Textf("Set %s and saved config. Provider/memory/channel changes take effect after a gateway restart.", key)
}
