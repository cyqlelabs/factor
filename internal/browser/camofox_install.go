//go:build !nobrowser

package browser

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// The headless engine is an npm package and a Firefox build it downloads on
// install. Both versions are pinned: a browser that changes under a running
// install is how a page that read yesterday stops reading today.
const (
	camofoxPackage = "@askjo/camofox-browser"
	camofoxVersion = "1.14.0"

	// nodeVersion is the Node the package is run on when the machine has
	// none new enough. The package itself wants 22; the floor is where
	// node:sqlite stopped needing a flag, which the SQLite stand-in below
	// is built on.
	nodeVersion    = "22.23.2"
	nodeMinVersion = "22.13"
)

// nodeDist is where Node's official builds and their checksums come from. A
// var so a test can serve its own.
var nodeDist = "https://nodejs.org/dist/v%s/"

var provisionMu sync.Mutex

// ProvisionInBackground installs the headless engine while the gateway
// starts, so an install that has never run the wizard — one upgraded from
// a Factor without the engine — has it before the first page is asked for
// rather than waiting a gigabyte of download in front of a reply. It also
// clears the read-only engine earlier versions kept, which nothing reads
// any more. Nothing here blocks startup, and a failure is a log line: the
// session falls back to a headless Chromium on its own.
func ProvisionInBackground(ctx context.Context, cfg config.BrowserConfig, home string) {
	if !cfg.Enabled || cfg.Engine == "chromium" || cfg.Camofox.Dir != "" {
		return
	}
	if old := filepath.Join(home, "engine", "lightpanda"); executable(old) {
		if err := os.Remove(old); err == nil {
			slog.Info("browser: removed the Lightpanda engine earlier versions installed; Camofox reads pages now")
		}
	}
	go func() {
		installCtx, cancel := context.WithTimeout(ctx, InstallTimeout)
		defer cancel()
		_, installed, err := provisionCamofox(installCtx, home, func(format string, args ...any) {
			slog.Info("browser: " + fmt.Sprintf(format, args...))
		})
		switch {
		case err != nil:
			slog.Warn("browser: Camofox could not be provisioned; headless browsing runs on the Chromium engine until it is", "error", err)
		case installed:
			slog.Info("browser: Camofox is installed and ready")
		}
	}()
}

// CamofoxDir is where the package is installed.
func CamofoxDir(home string) string { return filepath.Join(home, "engine", "camofox") }

// camofoxServer is the package's entry point inside dir.
func camofoxServer(dir string) string {
	return filepath.Join(dir, "node_modules", "@askjo", "camofox-browser", "server.js")
}

// camoufoxDir is where the Firefox build is kept: under the package rather
// than in the user's cache, so an install leaves with its directory.
func camoufoxDir(dir string) string { return filepath.Join(dir, "camoufox") }

// nodeDir is where a Node Factor provisioned lives.
func nodeDir(home string) string { return filepath.Join(home, "engine", "node") }

// provisionedNode is the interpreter inside nodeDir.
func provisionedNode(home string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(nodeDir(home), "node.exe")
	}
	return filepath.Join(nodeDir(home), "bin", "node")
}

// EnsureCamofox installs the headless engine when it is missing and returns
// the server to run. It needs Node: the one on PATH when it is new enough,
// the one Factor provisioned before, or one downloaded now. The wizard runs
// it unconditionally and the engine runs it on first use, so a machine set
// up over ssh and one that never ran the wizard both end with a browser.
func EnsureCamofox(ctx context.Context, home string, progress Progress) (string, bool, error) {
	// One install at a time: the gateway provisions at startup and the
	// engine provisions on first use, and the second to arrive waits for
	// the first rather than running npm beside it.
	provisionMu.Lock()
	defer provisionMu.Unlock()
	if progress == nil {
		progress = func(string, ...any) {}
	}
	dir := CamofoxDir(home)
	server := camofoxServer(dir)
	if _, err := os.Stat(server); err == nil && camoufoxFetched(dir) {
		return server, false, nil
	}
	node, err := ensureNode(ctx, home, progress)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	progress("installing Camofox %s (a 300 MB Firefox build downloads with it)", camofoxVersion)
	argv := []string{npmFor(node), "install", "--prefix", dir, "--no-fund", "--no-audit", "--loglevel=error",
		camofoxPackage + "@" + camofoxVersion}
	env := append(os.Environ(),
		"CAMOUFOX_INSTALL_DIR="+camoufoxDir(dir),
		"CAMOFOX_CRASH_REPORT_ENABLED=false",
		"npm_config_update_notifier=false",
	)
	if out, err := runCmdEnv(ctx, argv, env, dir); err != nil {
		return "", false, fmt.Errorf("installing Camofox: %v: %s", err, firstLine(out))
	}
	if _, err := os.Stat(server); err != nil {
		return "", false, fmt.Errorf("npm reported success but %s is missing", server)
	}
	if err := standIns(ctx, node, dir, progress); err != nil {
		return "", false, err
	}
	if !camoufoxFetched(dir) {
		// The package fetches the browser in its postinstall step, which
		// fails wherever impit does not load and which npm skips in some
		// configurations; asking the fetcher directly is the documented
		// fallback, and it runs after the stub above so it can.
		progress("fetching the Camoufox browser build")
		fetch := []string{node, filepath.Join(dir, "node_modules", "camoufox-js", "dist", "__main__.js"), "fetch"}
		if out, err := runCmdEnv(ctx, fetch, env, dir); err != nil || !camoufoxFetched(dir) {
			return "", false, fmt.Errorf("fetching the Camoufox build failed: %v: %s", err, firstLine(out))
		}
	}
	progress("installed Camofox %s", camofoxVersion)
	return server, true, nil
}

// A stand-in replaces a native module the machine cannot load with a pure
// JavaScript one that covers what the engine asks of it. Two of the package's
// dependencies ship prebuilt bindings that want a glibc two years newer than
// the distributions Factor is most often installed on, while the browser
// itself needs nothing newer than 2.18 — so without this the engine would be
// lost on exactly the boxes it is for, over code paths Factor never takes.
// Nothing is compiled: a toolchain is the one thing that cannot be assumed
// on every platform.
type standIn struct {
	module string // what is required
	file   string // the file inside node_modules that answers the require
	body   string // what stands in for it
}

var standInList = []standIn{
	{
		// camoufox-js imports impit at load time and uses it for one thing:
		// reading the public IP behind a proxy, which Factor never sets.
		module: "impit",
		file:   filepath.Join("impit", "index.wrapper.js"),
		body: `// Written by Factor: the native binding does not load on this machine.
// camoufox-js imports it at load time and only uses it to look up the public
// IP behind a proxy, which Factor does not configure.
class Impit { fetch() { return Promise.reject(new Error("impit is unavailable on this machine")); } }
module.exports = { Impit, HttpMethod: {}, Browser: {} };
`,
	},
	{
		// camoufox-js samples a WebGL fingerprint from a SQLite file on
		// every launch through better-sqlite3. Node has carried the same
		// synchronous API since 22.13, so the stand-in is an adapter.
		module: "better-sqlite3",
		file:   filepath.Join("better-sqlite3", "lib", "index.js"),
		body: `// Written by Factor: the native binding does not load on this machine.
// camoufox-js reads a fingerprint database through prepare().all(), which
// Node's own SQLite answers the same way.
const { DatabaseSync } = require("node:sqlite");
class Database extends DatabaseSync {
  constructor(path, options) {
    super(path, options && options.readonly ? { readOnly: true } : {});
  }
}
module.exports = Database;
module.exports.default = Database;
`,
	},
}

// standIns probes each native module the engine depends on and stands in
// for the ones that do not load. The original file is kept beside it.
func standIns(ctx context.Context, node, dir string, progress Progress) error {
	for _, si := range standInList {
		path := filepath.Join(dir, "node_modules", si.file)
		if _, err := os.Stat(path); err != nil {
			continue // the version installed does not carry it
		}
		out, err := runCmdEnv(ctx, []string{node, "-e", `require("` + si.module + `")`}, os.Environ(), dir)
		if err == nil {
			continue
		}
		progress("%s does not load here (%s); standing in for it, since Factor never needs what it does", si.module, firstLine(out))
		_ = os.Remove(path + ".orig")
		if err := os.Rename(path, path+".orig"); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(si.body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// camoufoxFetched reports whether the browser build is in place. The
// fetcher writes version.json last, on every platform, so its presence says
// the whole bundle arrived without knowing where each platform keeps the
// executable.
func camoufoxFetched(dir string) bool {
	_, err := os.Stat(filepath.Join(camoufoxDir(dir), "version.json"))
	return err == nil
}

// npmFor is the npm that ships beside a node binary. A node found on PATH
// answers with the npm on PATH.
func npmFor(node string) string {
	if filepath.Base(node) == "node" || filepath.Base(node) == "node.exe" {
		if dir := filepath.Dir(node); dir != "." && dir != "" {
			name := "npm"
			if runtime.GOOS == "windows" {
				name = "npm.cmd"
			}
			if candidate := filepath.Join(dir, name); executable(candidate) {
				return candidate
			}
		}
	}
	if runtime.GOOS == "windows" {
		return "npm.cmd"
	}
	return "npm"
}

// runCmdEnv is runCmd with an environment and a working directory.
var runCmdEnv = func(ctx context.Context, argv, env []string, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// findNode returns a Node new enough to run the package: the one on PATH,
// or the one Factor provisioned.
func findNode(home string) (string, error) {
	if path, err := lookPath("node"); err == nil {
		if have, err := nodeVersionOf(context.Background(), path); err == nil && versionAtLeast(have, nodeMinVersion) {
			return path, nil
		}
	}
	if own := provisionedNode(home); executable(own) {
		return own, nil
	}
	return "", fmt.Errorf("no Node %s or newer on this machine", nodeMinVersion)
}

var nodeVersionPattern = regexp.MustCompile(`v?(\d+\.\d+)`)

// nodeVersionOf asks a node binary for its major.minor.
func nodeVersionOf(ctx context.Context, node string) (string, error) {
	out, err := runCmd(ctx, []string{node, "--version"})
	if err != nil {
		return "", err
	}
	m := nodeVersionPattern.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("node --version said %q", firstLine(out))
	}
	return m[1], nil
}

// ensureNode returns a usable Node, downloading the pinned release into the
// engine directory when the machine has none. The archive is checked against
// the release's SHASUMS256 before it is unpacked.
func ensureNode(ctx context.Context, home string, progress Progress) (string, error) {
	if path, err := findNode(home); err == nil {
		return path, nil
	}
	asset, err := nodeAsset()
	if err != nil {
		return "", err
	}
	progress("downloading Node %s (%s)", nodeVersion, asset)
	base := fmt.Sprintf(nodeDist, nodeVersion)
	want, err := nodeChecksum(ctx, base+"SHASUMS256.txt", asset)
	if err != nil {
		return "", err
	}
	staging := filepath.Join(home, "engine", ".staging-node")
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	archive := filepath.Join(staging, asset)
	if err := download(ctx, base+asset, archive, 0, progress); err != nil {
		return "", fmt.Errorf("downloading Node: %w", err)
	}
	if got, err := fileSHA256(archive); err != nil || got != want {
		return "", fmt.Errorf("the Node download does not match its checksum (got %s, want %s)", got, want)
	}
	unpacked := filepath.Join(staging, "unpacked")
	if strings.HasSuffix(asset, ".zip") {
		err = unzip(archive, unpacked)
	} else {
		err = untarGz(archive, unpacked)
	}
	if err != nil {
		return "", fmt.Errorf("unpacking Node: %w", err)
	}
	root, err := singleDir(unpacked)
	if err != nil {
		return "", err
	}
	_ = os.RemoveAll(nodeDir(home))
	if err := os.Rename(root, nodeDir(home)); err != nil {
		return "", err
	}
	node := provisionedNode(home)
	have, err := nodeVersionOf(ctx, node)
	if err != nil {
		return "", fmt.Errorf("the Node build for this machine will not run: %v", err)
	}
	if !versionAtLeast(have, nodeMinVersion) {
		return "", fmt.Errorf("the Node build unpacked reports v%s, not the %s it should be", have, nodeVersion)
	}
	progress("installed Node %s", nodeVersion)
	return node, nil
}

// nodeAsset names the official archive for this machine.
func nodeAsset() (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		return "", fmt.Errorf("no Node build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "linux":
		return fmt.Sprintf("node-v%s-linux-%s.tar.gz", nodeVersion, arch), nil
	case "darwin":
		return fmt.Sprintf("node-v%s-darwin-%s.tar.gz", nodeVersion, arch), nil
	case "windows":
		return fmt.Sprintf("node-v%s-win-%s.zip", nodeVersion, arch), nil
	}
	return "", fmt.Errorf("no Node build for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// nodeChecksum reads the release's checksum for one asset.
func nodeChecksum(ctx context.Context, url, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reading the Node checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading the Node checksums: %s", resp.Status)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("the Node release lists no checksum for %s", asset)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// untarGz unpacks a gzipped tarball under dest, refusing entries that would
// land outside it.
func untarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// unzip unpacks a zip archive under dest, refusing entries that would land
// outside it.
func unzip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, f := range r.File {
		target, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()&0o777|0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// safeJoin resolves an archive entry under dest and refuses one that
// escapes it.
func safeJoin(dest, name string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(name))
	if target != dest && !strings.HasPrefix(target, dest+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return target, nil
}
