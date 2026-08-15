//go:build !nobrowser

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// A browser is not optional garnish: without one the whole suite is dead
// weight, and the machines Factor targets routinely have none. Old desktops
// ship a Firefox derivative and nothing else; Puppy Linux carries
// chromium-browser only as a snap stub that installs nothing usable. So
// `factor init` provisions a browser itself rather than leaving the user to
// discover the gap through a failed tool call.
//
// Helium is the pick. It is an ungoogled-chromium downstream that strips
// Google's telemetry and background services, carries the Bromite/Iridium/
// Brave/Inox anti-fingerprinting patches, and bundles uBlock Origin — which
// is what actually keeps a tab's memory down on a small box. It ships a
// portable tarball, so provisioning needs no package manager and no root.
// It is still Chromium underneath, so CDP and a real renderer come for free:
// nothing outside that family offers both.

const (
	heliumRepo     = "imputnet/helium-linux"
	heliumHome     = "https://helium.computer"
	lightpandaRepo = "lightpanda-io/browser"

	// InstallTimeout bounds one provisioning attempt. The tarball is ~125MB
	// and the machines that need it most are the ones on slow links.
	InstallTimeout = 20 * time.Minute
)

// Overridable seams so the whole decision tree is testable without touching
// the machine or the network.
var (
	releaseAPI = "https://api.github.com/repos/%s/releases/latest"
	httpClient = &http.Client{Timeout: InstallTimeout}
	lookPath   = exec.LookPath
	runCmd     = func(ctx context.Context, argv []string) (string, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
)

// Progress reports provisioning steps to whoever is watching (the wizard
// prints them; the gateway logs them).
type Progress func(format string, args ...any)

// EngineDir is where a provisioned browser lives.
func EngineDir(home string) string { return filepath.Join(home, "engine", "helium") }

// EngineBinary is the provisioned browser's executable.
func EngineBinary(home string) string {
	name := "helium"
	if runtime.GOOS == "windows" {
		name = "helium.exe"
	}
	return filepath.Join(EngineDir(home), name)
}

// EnsureEngine returns a Chromium-family binary the browser suite can drive,
// downloading Helium when the machine has none. The bool reports whether this
// call installed it. Callers holding a configured browser.command should
// resolve that with FindBrowserBinary first — this function only knows about
// the machine, not the config.
func EnsureEngine(ctx context.Context, home string, progress Progress) (string, bool, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	if path, err := FindBrowserBinary(""); err == nil {
		return path, false, nil
	}
	// Discovery searches $FACTOR_HOME; provisioning was asked to use this
	// one. They are the same directory in every normal run, and checking
	// both is what keeps an unusual one from re-downloading 120MB.
	if path := EngineBinary(home); executable(path) {
		return path, false, nil
	}
	if runtime.GOOS != "linux" {
		return "", false, fmt.Errorf("no Chromium-family browser found, and this release only provisions one automatically on Linux — install Chrome, Chromium, Edge, or Helium (%s)", heliumHome)
	}
	path, err := installHelium(ctx, home, progress)
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}

func installHelium(ctx context.Context, home string, progress Progress) (string, error) {
	asset, version, err := heliumAsset(ctx)
	if err != nil {
		return "", err
	}
	if _, err := lookPath("tar"); err != nil {
		return "", fmt.Errorf("tar is needed to unpack Helium and is not installed: %w", err)
	}

	staging := filepath.Join(home, "engine", ".staging")
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	archive := filepath.Join(staging, asset.Name)
	progress("downloading Helium %s (%d MB)", version, asset.Size>>20)
	if err := download(ctx, asset.URL, archive, asset.Size, progress); err != nil {
		return "", err
	}

	progress("unpacking")
	if out, err := runCmd(ctx, []string{"tar", "-xJf", archive, "-C", staging}); err != nil {
		return "", fmt.Errorf("unpacking Helium: %v: %s", err, strings.TrimSpace(out))
	}
	unpacked, err := singleDir(staging)
	if err != nil {
		return "", err
	}

	target := EngineDir(home)
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := os.Rename(unpacked, target); err != nil {
		return "", fmt.Errorf("installing Helium into %s: %w", target, err)
	}

	binary := EngineBinary(home)
	out, err := runCmd(ctx, []string{binary, "--version"})
	if err != nil {
		return "", fmt.Errorf("the Helium just installed will not run here: %v: %s", err, strings.TrimSpace(out))
	}
	progress("installed %s", strings.TrimSpace(out))
	return binary, nil
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type ghRelease struct {
	Tag    string    `json:"tag_name"`
	Assets []ghAsset `json:"assets"`
}

// latestAsset finds the newest release asset named for this machine.
func latestAsset(ctx context.Context, repo, name, want string) (ghAsset, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(releaseAPI, repo), nil)
	if err != nil {
		return ghAsset{}, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return ghAsset{}, "", fmt.Errorf("looking up the latest %s release: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ghAsset{}, "", fmt.Errorf("looking up the latest %s release: %s", name, resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return ghAsset{}, "", fmt.Errorf("reading the %s release index: %w", name, err)
	}
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, want) {
			return a, rel.Tag, nil
		}
	}
	return ghAsset{}, "", fmt.Errorf("the latest %s release has no %s build", name, want)
}

// heliumAsset picks the portable tarball for this architecture. The tarball
// is preferred over the AppImage because it needs no FUSE, which the small
// distributions Factor targets often lack.
func heliumAsset(ctx context.Context) (ghAsset, string, error) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		return ghAsset{}, "", fmt.Errorf("no Helium build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return latestAsset(ctx, heliumRepo, "Helium", arch+"_linux.tar.xz")
}

// FastEngineBinary is the provisioned lightweight engine.
func FastEngineBinary(home string) string {
	return filepath.Join(home, "engine", "lightpanda")
}

// EnsureFastEngine installs Lightpanda: a browser written from scratch for
// automation, with a CDP server and no renderer at all. It reads pages for a
// fraction of Chromium's memory, which is worth a second engine on a small
// box — but only reads them, so it supplements the real browser instead of
// replacing it. Its builds need glibc 2.34, which older distributions do not
// have; the version check below is what turns that into a clear answer.
func EnsureFastEngine(ctx context.Context, home string, progress Progress) (string, bool, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	binary := FastEngineBinary(home)
	if executable(binary) {
		return binary, false, nil
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	osName := map[string]string{"linux": "linux", "darwin": "macos"}[runtime.GOOS]
	if arch == "" || osName == "" {
		return "", false, fmt.Errorf("no Lightpanda build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	// Ask before downloading. Finding out from the dynamic linker costs
	// 150MB and several minutes on exactly the slow machines least able to
	// spare either.
	if ok, why := FastEngineSupported(); !ok {
		return "", false, errors.New(why)
	}
	asset, version, err := latestAsset(ctx, lightpandaRepo, "Lightpanda", "lightpanda-"+arch+"-"+osName)
	if err != nil {
		return "", false, err
	}

	staging := filepath.Join(home, "engine", ".staging-fast")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", false, err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	tmp := filepath.Join(staging, asset.Name)
	progress("downloading Lightpanda %s (%d MB)", version, asset.Size>>20)
	if err := download(ctx, asset.URL, tmp, asset.Size, progress); err != nil {
		return "", false, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return "", false, err
	}
	if err := os.Rename(tmp, binary); err != nil {
		return "", false, fmt.Errorf("installing Lightpanda into %s: %w", binary, err)
	}
	out, err := runCmd(ctx, []string{binary, "version"})
	if err != nil {
		_ = os.Remove(binary)
		return "", false, fmt.Errorf("the Lightpanda build for this machine will not run: %v: %s", err, firstLine(out))
	}
	progress("installed Lightpanda %s", strings.TrimSpace(out))
	return binary, true, nil
}

// lightpandaMinGlibc is the floor its official builds link against. Anything
// older cannot load them, whatever the architecture says.
const lightpandaMinGlibc = "2.34"

// FastEngineSupported reports whether this machine can run the lightweight
// engine, and why not when it cannot.
func FastEngineSupported() (bool, string) {
	if runtime.GOOS != "linux" {
		return true, ""
	}
	have := glibcVersion()
	switch {
	case have == "musl":
		return false, "the lightweight engine is built against glibc and this system uses musl"
	case have == "":
		return true, "" // undetermined: let the install try and report for itself
	case !versionAtLeast(have, lightpandaMinGlibc):
		return false, fmt.Sprintf("the lightweight engine needs glibc %s and this system has %s", lightpandaMinGlibc, have)
	}
	return true, ""
}

// glibcVersion reads the C library version from ldd, returning "musl" for
// musl systems and "" when it cannot tell.
func glibcVersion() string {
	out, err := runCmd(context.Background(), []string{"ldd", "--version"})
	if err != nil && out == "" {
		return ""
	}
	line := firstLine(out)
	if strings.Contains(strings.ToLower(line), "musl") {
		return "musl"
	}
	// "ldd (Ubuntu GLIBC 2.31-0ubuntu9) 2.31" — the trailing field is the
	// plain version, so the last match is the one to take.
	matches := versionPattern.FindAllString(line, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

var versionPattern = regexp.MustCompile(`\d+\.\d+`)

// versionAtLeast compares dotted major.minor versions.
func versionAtLeast(have, want string) bool {
	h, w := strings.SplitN(have, ".", 2), strings.SplitN(want, ".", 2)
	if len(h) < 2 || len(w) < 2 {
		return false
	}
	hMaj, err1 := strconv.Atoi(h[0])
	hMin, err2 := strconv.Atoi(h[1])
	wMaj, err3 := strconv.Atoi(w[0])
	wMin, err4 := strconv.Atoi(w[1])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return false
	}
	if hMaj != wMaj {
		return hMaj > wMaj
	}
	return hMin >= wMin
}

// firstLine keeps a loader's complaint readable: the dynamic linker prints
// one line per missing symbol version, and they all say the same thing.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func download(ctx context.Context, url, dest string, size int64, progress Progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading Helium: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading Helium: %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, &progressReader{r: resp.Body, total: size, report: progress}); err != nil {
		return fmt.Errorf("downloading Helium: %w", err)
	}
	return f.Close()
}

// progressReader reports every 10% so a slow link still shows movement
// without flooding the terminal.
type progressReader struct {
	r      io.Reader
	total  int64
	read   int64
	steps  int64
	report Progress
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.total > 0 {
		if step := p.read * 10 / p.total; step > p.steps {
			p.steps = step
			p.report("downloaded %d%%", step*10)
		}
	}
	return n, err
}

// singleDir returns the one directory a well-formed tarball unpacks into.
func singleDir(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var found string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("the Helium archive unpacked into several directories")
		}
		found = filepath.Join(root, e.Name())
	}
	if found == "" {
		return "", fmt.Errorf("the Helium archive unpacked no directory")
	}
	return found, nil
}

// wellKnownBrowsers are the fixed locations a desktop session's PATH never
// covers: macOS app bundles, Windows installs, and the copy Factor
// provisions itself. It is a var because tests that need a machine with no
// browser at all cannot clear those locations, only replace the lookup.
var wellKnownBrowsers = func() []string {
	paths := []string{EngineBinary(config.Home())}
	switch runtime.GOOS {
	case "darwin":
		paths = append(paths,
			"/Applications/Helium.app/Contents/MacOS/Helium",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		)
	case "windows":
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			if root == "" {
				continue
			}
			paths = append(paths,
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(root, "Chromium", "Application", "chrome.exe"),
			)
		}
	}
	return paths
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode()&0o111 != 0
}
