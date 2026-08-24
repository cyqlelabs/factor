package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// managerSpec describes one supported package manager.
type managerSpec struct {
	probe   string   // binary that must exist
	install []string // command template; packages appended
	system  bool     // needs root (sudo -n when not root)
	// oneAtATime is for managers whose install verb takes a single package.
	// winget reads everything after the verb as one query, so a list would
	// install nothing and report success on whatever it matched first.
	oneAtATime bool
}

var managerSpecs = map[string]managerSpec{
	"apt":    {probe: "apt-get", install: []string{"apt-get", "install", "-y"}, system: true},
	"apk":    {probe: "apk", install: []string{"apk", "add"}, system: true},
	"dnf":    {probe: "dnf", install: []string{"dnf", "install", "-y"}, system: true},
	"pacman": {probe: "pacman", install: []string{"pacman", "-S", "--noconfirm"}, system: true},
	"xbps":   {probe: "xbps-install", install: []string{"xbps-install", "-y"}, system: true},
	// Puppy Linux woof-CE. Its `install` verb only unpacks something already
	// downloaded — `get` is the one that fetches and installs — and the flag
	// has to precede the verb or it is read as a package name.
	"pkg":  {probe: "pkg", install: []string{"pkg", "-f", "get"}, system: true},
	"pip":  {probe: "pip", install: []string{"pip", "install"}},
	"pipx": {probe: "pipx", install: []string{"pipx", "install"}},
	"uv":   {probe: "uv", install: []string{"uv", "tool", "install"}},
	"npm":  {probe: "npm", install: []string{"npm", "install", "-g"}},
	// Windows. winget ships with Windows 10 1709 and later, elevates on its
	// own when a package needs it, and is the only system manager a stock
	// Windows machine has - without it setup could do nothing there but print
	// a download link. The agreement flags matter: it stops for a prompt
	// nobody is watching otherwise.
	"winget": {probe: "winget", install: []string{"winget", "install", "--exact", "--silent",
		"--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity"},
		oneAtATime: true},
}

// autoOrder is the probe order for system managers in auto mode.
var autoOrder = []string{"apt", "apk", "dnf", "pacman", "xbps", "pkg", "winget"}

// DetectSystemManager returns the system package manager available on this
// machine ("" when none of the supported ones is installed). The wizard uses
// it to name the exact packages a distribution needs.
func DetectSystemManager() string {
	for _, name := range autoOrder {
		if _, err := exec.LookPath(managerSpecs[name].probe); err == nil {
			return name
		}
	}
	return ""
}

// PkgInstallTool installs software so the agent can extend its own
// environment (e.g. `pip install smrti`, MCP servers, CLI utilities).
type PkgInstallTool struct {
	lookPath func(string) (string, error)
	runner   func(ctx context.Context, argv []string) (string, error)
	euid     func() int
}

func NewPkgInstallTool() *PkgInstallTool {
	return &PkgInstallTool{
		lookPath: exec.LookPath,
		euid:     os.Geteuid,
		runner: func(ctx context.Context, argv []string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.WaitDelay = 5 * time.Second
			setProcessGroup(cmd)
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
	}
}

func (t *PkgInstallTool) Name() string { return "pkg_install" }
func (t *PkgInstallTool) Description() string {
	return "Install packages. manager=auto detects the system package manager (apt/apk/dnf/pacman/xbps/pkg); or pick pip/pipx/uv/npm for language packages. Confirm with the user before installing anything they didn't ask for."
}
func (t *PkgInstallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"packages": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Package names as the chosen manager spells them (e.g. python3-pip for apt, pillow for pip)"},
			"manager":  map[string]any{"type": "string", "enum": []any{"auto", "apt", "apk", "dnf", "pacman", "xbps", "pkg", "pip", "pipx", "uv", "npm"}, "description": "Which manager to use (default auto, which picks the system one). Choose pip/pipx/uv/npm explicitly for language packages"},
		},
		"required": []any{"packages"},
	}
}

func (t *PkgInstallTool) resolve(manager string) (string, managerSpec, error) {
	if manager == "" || manager == "auto" {
		for _, name := range autoOrder {
			spec := managerSpecs[name]
			if _, err := t.lookPath(spec.probe); err == nil {
				return name, spec, nil
			}
		}
		return "", managerSpec{}, fmt.Errorf("no supported system package manager found (tried %s)", strings.Join(autoOrder, ", "))
	}
	spec, ok := managerSpecs[manager]
	if !ok {
		return "", managerSpec{}, fmt.Errorf("unsupported manager %q", manager)
	}
	if _, err := t.lookPath(spec.probe); err != nil {
		return "", managerSpec{}, fmt.Errorf("%s is not installed on this system", spec.probe)
	}
	return manager, spec, nil
}

var pkgNameOK = func(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, " \t\n;|&$`\"'\\")
}

func (t *PkgInstallTool) Execute(ctx context.Context, args map[string]any) *Result {
	raw, _ := args["packages"].([]any)
	var packages []string
	for _, p := range raw {
		s, _ := p.(string)
		if !pkgNameOK(s) {
			return Errorf("invalid package name %q", s)
		}
		packages = append(packages, s)
	}
	if len(packages) == 0 {
		return Errorf("packages must be a non-empty list of names")
	}

	name, spec, err := t.resolve(StringArg(args, "manager"))
	if err != nil {
		return Errorf("%v", err)
	}

	argv := append([]string{}, spec.install...)
	elevated := false
	if spec.system && t.euid() != 0 {
		if _, err := t.lookPath("sudo"); err != nil {
			return Errorf("%s needs root and sudo is unavailable; ask the user to install manually", name)
		}
		argv = append([]string{"sudo", "-n"}, argv...)
		elevated = true
	}
	// A manager that takes one package per call is run once per package, and
	// the first failure is the one reported: carrying on would install the
	// rest and still have to fail, which reads as a partial success nobody
	// asked for.
	var out, failed string
	if spec.oneAtATime {
		for _, pkg := range packages {
			var o string
			o, err = t.runner(ctx, append(append([]string{}, argv...), pkg))
			out += o
			if err != nil {
				failed = pkg
				argv = append(argv, pkg) // the one that failed is the one to suggest
				break
			}
		}
	} else {
		argv = append(argv, packages...)
		out, err = t.runner(ctx, argv)
	}
	if len(out) > 8*1024 {
		out = out[len(out)-8*1024:]
	}
	if err != nil {
		if strings.Contains(out, "a password is required") {
			// Suggest the command without our non-interactive sudo prefix.
			suggestion := argv
			if elevated {
				suggestion = argv[2:]
			}
			return Errorf("%s needs an interactive sudo password; ask the user to run: sudo %s",
				name, strings.Join(suggestion, " "))
		}
		if failed != "" {
			return Errorf("installing %s failed via %s: %v\n%s", failed, name, err, out)
		}
		return Errorf("install failed via %s: %v\n%s", name, err, out)
	}
	return Textf("Installed %s via %s.\n%s", strings.Join(packages, ", "), name, tail(out, 2000))
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
