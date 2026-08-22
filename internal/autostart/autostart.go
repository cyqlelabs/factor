// Package autostart puts Factor into the user's login sequence: a systemd
// user service where a user manager is running, an XDG autostart entry on
// other Linux desktops, a launchd agent on macOS, and a Run registry value on
// Windows. The wizard installs these itself — a missing autostart is a setup
// step, not homework for the user.
package autostart

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/desktop"
)

// Entry names an installed login entry: the mechanism in words the summary
// can print, and the file (or registry value) that carries it.
type Entry struct {
	Mechanism string
	Path      string
}

const (
	unitName    = "factor.service"
	desktopName = "factor.desktop"
	plistName   = "com.cyqlelabs.factor.plist"
	launchLabel = "com.cyqlelabs.factor"
)

// Install writes the login entry for this platform and enables it. exe is the
// binary to start; configPath, when not blank, is carried into the entry so a
// gateway born at login reads the same config the wizard wrote.
func Install(ctx context.Context, env desktop.Env, exe, configPath string) (Entry, error) {
	switch env.GOOS {
	case "windows":
		return installWindows(exe, configPath)
	case "darwin":
		return installLaunchd(ctx, env, exe, configPath)
	default:
		if systemdAvailable(ctx, env) {
			return installSystemd(ctx, env, exe, configPath)
		}
		return installXDG(env, exe, configPath)
	}
}

// Uninstall removes whatever Install put in place. A platform may hold more
// than one flavour (a box that gained systemd after an XDG install), so every
// flavour it knows is cleaned, and an absent entry is not an error.
func Uninstall(ctx context.Context, env desktop.Env) error {
	switch env.GOOS {
	case "windows":
		return uninstallWindows()
	case "darwin":
		return uninstallLaunchd(ctx, env)
	default:
		return uninstallLinux(ctx, env)
	}
}

// Installed reports the login entry currently in place, if any.
func Installed(env desktop.Env) (Entry, bool) {
	switch env.GOOS {
	case "windows":
		return installedWindows()
	case "darwin":
		path, err := launchAgentPath()
		if err != nil {
			return Entry{}, false
		}
		return fileEntry("launchd agent", path)
	default:
		if entry, ok := fileEntry("systemd user service", filepath.Join(configDir(env), "systemd", "user", unitName)); ok {
			return entry, ok
		}
		return fileEntry("XDG autostart entry", filepath.Join(configDir(env), "autostart", desktopName))
	}
}

func fileEntry(mechanism, path string) (Entry, bool) {
	if _, err := os.Stat(path); err != nil {
		return Entry{}, false
	}
	return Entry{Mechanism: mechanism, Path: path}, true
}

// ---- linux -----------------------------------------------------------------

// systemdAvailable reports whether a systemd user manager answers for this
// login. `is-system-running` exits non-zero on a merely degraded manager, so
// the state on stdout is the verdict, not the exit code — no user bus at all
// leaves stdout empty.
func systemdAvailable(ctx context.Context, env desktop.Env) bool {
	if env.Has == nil || !env.Has("systemctl") {
		return false
	}
	out, _ := run(ctx, env, "systemctl", "--user", "is-system-running")
	state := strings.TrimSpace(out)
	return state != "" && state != "offline"
}

func installSystemd(ctx context.Context, env desktop.Env, exe, configPath string) (Entry, error) {
	path := filepath.Join(configDir(env), "systemd", "user", unitName)
	if err := writeFile(path, unitText(exe, configPath)); err != nil {
		return Entry{}, err
	}
	for _, args := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", unitName},
	} {
		if _, err := run(ctx, env, args...); err != nil {
			return Entry{}, fmt.Errorf("enabling the user service: %w", err)
		}
	}
	return Entry{Mechanism: "systemd user service", Path: path}, nil
}

// unitText is the user service. The gateway runs in the foreground here —
// systemd is the supervisor, so the self-detaching -d would only hide the
// process from it.
func unitText(exe, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Factor gateway

[Service]
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, gatewayCommand(exe, configPath, false))
}

func installXDG(env desktop.Env, exe, configPath string) (Entry, error) {
	path := filepath.Join(configDir(env), "autostart", desktopName)
	if err := writeFile(path, desktopText(exe, configPath)); err != nil {
		return Entry{}, err
	}
	return Entry{Mechanism: "XDG autostart entry", Path: path}, nil
}

// desktopText is the XDG autostart entry. Nothing supervises it, so the
// gateway detaches itself with -d and logs where -d logs.
func desktopText(exe, configPath string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Factor
Comment=Factor gateway
Exec=%s
X-GNOME-Autostart-enabled=true
`, gatewayCommand(exe, configPath, true))
}

func uninstallLinux(ctx context.Context, env desktop.Env) error {
	unit := filepath.Join(configDir(env), "systemd", "user", unitName)
	if _, err := os.Stat(unit); err == nil {
		// Best effort: the entry is gone once the file is, even if a broken
		// user manager cannot be told about it right now.
		_, _ = run(ctx, env, "systemctl", "--user", "disable", unitName)
		if err := os.Remove(unit); err != nil {
			return err
		}
		_, _ = run(ctx, env, "systemctl", "--user", "daemon-reload")
	}
	return removeIfPresent(filepath.Join(configDir(env), "autostart", desktopName))
}

// ---- darwin ----------------------------------------------------------------

func installLaunchd(ctx context.Context, env desktop.Env, exe, configPath string) (Entry, error) {
	path, err := launchAgentPath()
	if err != nil {
		return Entry{}, err
	}
	if err := writeFile(path, plistText(exe, configPath)); err != nil {
		return Entry{}, err
	}
	if _, err := run(ctx, env, "launchctl", "load", "-w", path); err != nil {
		return Entry{}, fmt.Errorf("loading the launch agent: %w", err)
	}
	return Entry{Mechanism: "launchd agent", Path: path}, nil
}

// plistText is the launch agent. launchd supervises and owns the output, so
// the gateway runs in the foreground and its log lands where -d would put it.
func plistText(exe, configPath string) string {
	args := []string{exe, "gateway"}
	if configPath != "" {
		args = append(args, "-c", configPath)
	}
	var argv strings.Builder
	for _, a := range args {
		fmt.Fprintf(&argv, "\t\t<string>%s</string>\n", xmlEscape(a))
	}
	log := xmlEscape(filepath.Join(config.Home(), "gateway.log")) // the file gateway.LogPath names
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchLabel, argv.String(), log, log)
}

func uninstallLaunchd(ctx context.Context, env desktop.Env) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		_, _ = run(ctx, env, "launchctl", "unload", path)
	}
	return removeIfPresent(path)
}

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", plistName), nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- shared ----------------------------------------------------------------

// gatewayCommand renders the start command for a unit or entry, quoting the
// paths so a space in one survives the launcher's word splitting.
func gatewayCommand(exe, configPath string, detach bool) string {
	cmd := quotePath(exe) + " gateway"
	if detach {
		cmd += " -d"
	}
	if configPath != "" {
		cmd += " -c " + quotePath(configPath)
	}
	return cmd
}

func configDir(env desktop.Env) string {
	if env.Getenv != nil {
		if dir := env.Getenv("XDG_CONFIG_HOME"); dir != "" {
			return dir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config"
	}
	return filepath.Join(home, ".config")
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func run(ctx context.Context, env desktop.Env, argv ...string) (string, error) {
	if env.Run == nil {
		return "", fmt.Errorf("no command runner")
	}
	return env.Run(ctx, "", argv...)
}
