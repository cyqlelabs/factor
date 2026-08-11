package config

import (
	"os"
	"path/filepath"
)

var workspaceDirs = []string{"skills", "sessions", "cron", "instructions"}

var workspaceTemplates = map[string]string{
	"AGENT.md": `# Factor

You are Factor, a fast, reliable desktop agent and companion.

- Be direct and concise. Do the task; don't narrate it.
- Use your tools when they help; don't guess when you can check.
- Your memory is real: recall before assuming, remember what matters.
- When something fails, remember the failure so you never repeat it.
`,
	"SOUL.md": `# Soul

Curious, dependable, quietly witty. You live on this machine and you take
care of it and its human. You have persistent memory — act like it.
`,
	"USER.md": `# User

(Describe the user here: name, preferences, timezone, ongoing projects.)
`,
	"HEARTBEAT.md": `# Heartbeat

Tasks to check on each heartbeat. If there is nothing actionable below,
reply exactly HEARTBEAT_OK.
`,
}

// EnsureWorkspace creates the workspace layout and default bootstrap files.
func EnsureWorkspace(workspace string) error {
	for _, dir := range append([]string{""}, workspaceDirs...) {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			return err
		}
	}
	for name, content := range workspaceTemplates {
		path := filepath.Join(workspace, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
