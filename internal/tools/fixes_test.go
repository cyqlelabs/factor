package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A tool written in Go naturally spells required as []string; that must not
// silently disable required-argument checking.
func TestValidateArgsAcceptsGoStringSlices(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	}
	if err := ValidateArgs(schema, map[string]any{}); err == nil {
		t.Error("required []string was ignored")
	}
	if err := ValidateArgs(schema, map[string]any{"path": "a.txt"}); err != nil {
		t.Errorf("valid args rejected: %v", err)
	}

	// unsupported shapes are ignored rather than panicking
	odd := map[string]any{"required": 42}
	if err := ValidateArgs(odd, map[string]any{}); err != nil {
		t.Errorf("unsupported required shape errored: %v", err)
	}
}

func TestPkgInstallSudoHintMatchesTheManager(t *testing.T) {
	newTool := func(system bool) *PkgInstallTool {
		return &PkgInstallTool{
			lookPath: func(string) (string, error) { return "/usr/bin/x", nil },
			euid:     func() int { return 1000 },
			runner: func(context.Context, []string) (string, error) {
				return "sudo: a password is required", fmt.Errorf("exit 1")
			},
		}
	}

	// system manager: the hint drops our non-interactive sudo prefix
	res := newTool(true).Execute(context.Background(), map[string]any{
		"packages": []any{"htop"}, "manager": "apt",
	})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(res.ForLLM, "sudo apt-get install -y htop") {
		t.Errorf("system hint = %q", res.ForLLM)
	}

	// language manager: the command is never sliced, so it stays intact
	res = newTool(false).Execute(context.Background(), map[string]any{
		"packages": []any{"foo"}, "manager": "npm",
	})
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(res.ForLLM, "npm install -g foo") {
		t.Errorf("language-manager hint mangled the command: %q", res.ForLLM)
	}
}
