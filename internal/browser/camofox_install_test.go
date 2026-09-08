//go:build !nobrowser

package browser

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

// stubCommands replaces every process the installer runs, recording what it
// was asked, and answers node --version with the given major.
func stubCommands(t *testing.T, nodeMajor int, impitLoads bool) *[]string {
	t.Helper()
	var calls []string
	savedRun, savedRunEnv, savedLook := runCmd, runCmdEnv, lookPath
	runCmd = func(_ context.Context, argv []string) (string, error) {
		calls = append(calls, strings.Join(argv, " "))
		if len(argv) == 2 && argv[1] == "--version" {
			if argv[0] == "/usr/bin/node" {
				return fmt.Sprintf("v%d.14.0\n", nodeMajor), nil
			}
			return "v22.23.2\n", nil // a node Factor provisioned
		}
		return "", nil
	}
	runCmdEnv = func(_ context.Context, argv, env []string, dir string) (string, error) {
		calls = append(calls, strings.Join(argv, " ")+" | "+strings.Join(pick(env, "CAMOUFOX_INSTALL_DIR", "CAMOFOX_CRASH_REPORT_ENABLED"), " "))
		switch {
		case len(argv) > 1 && argv[1] == "install":
			// npm lays the package down.
			server := camofoxServer(dir)
			if err := os.MkdirAll(filepath.Dir(server), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(server, []byte("// server"), 0o644); err != nil {
				return "", err
			}
			for _, si := range standInList {
				native := filepath.Join(dir, "node_modules", si.file)
				if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
					return "", err
				}
				if err := os.WriteFile(native, []byte("native"), 0o644); err != nil {
					return "", err
				}
			}
			return "", nil
		case len(argv) > 2 && argv[1] == "-e" && strings.Contains(argv[2], "require"):
			if impitLoads {
				return "", nil
			}
			return "Error: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.33' not found", errors.New("exit status 1")
		case len(argv) > 2 && argv[2] == "fetch":
			if err := os.MkdirAll(camoufoxDir(dir), 0o755); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(camoufoxDir(dir), "version.json"), []byte(`{"version":"152.0.4"}`), 0o644)
		}
		return "", nil
	}
	lookPath = func(name string) (string, error) {
		if name == "node" && nodeMajor > 0 {
			return "/usr/bin/node", nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { runCmd, runCmdEnv, lookPath = savedRun, savedRunEnv, savedLook })
	return &calls
}

func pick(env []string, keys ...string) []string {
	var out []string
	for _, e := range env {
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				out = append(out, e)
			}
		}
	}
	return out
}

func TestEnsureCamofoxInstallsWithTheMachinesNode(t *testing.T) {
	home := t.TempDir()
	calls := stubCommands(t, 22, false)
	var progress []string
	server, installed, err := EnsureCamofox(context.Background(), home, func(f string, a ...any) { progress = append(progress, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	if !installed || server != camofoxServer(CamofoxDir(home)) {
		t.Errorf("server = %q installed = %v", server, installed)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "npm install --prefix "+CamofoxDir(home)) || !strings.Contains(joined, camofoxPackage+"@"+camofoxVersion) {
		t.Errorf("npm was not asked for the pinned package:\n%s", joined)
	}
	if !strings.Contains(joined, "CAMOUFOX_INSTALL_DIR="+camoufoxDir(CamofoxDir(home))) || !strings.Contains(joined, "CAMOFOX_CRASH_REPORT_ENABLED=false") {
		t.Errorf("the install ran without Factor's environment:\n%s", joined)
	}
	// Neither binding loaded, so the stand-ins are in place and the
	// originals kept beside them.
	for _, si := range standInList {
		path := filepath.Join(CamofoxDir(home), "node_modules", si.file)
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), "Written by Factor") {
			t.Errorf("%s was not stood in for: %q", si.module, data)
		}
		if _, err := os.Stat(path + ".orig"); err != nil {
			t.Errorf("the original %s was not kept", si.module)
		}
	}
	if joined := strings.Join(progress, "\n"); !strings.Contains(joined, "impit does not load here") || !strings.Contains(joined, "better-sqlite3 does not load here") {
		t.Errorf("the stand-ins were silent:\n%s", joined)
	}
	// The postinstall did not fetch the browser; the installer asked for it.
	if !strings.Contains(joined, "__main__.js fetch") || !camoufoxFetched(CamofoxDir(home)) {
		t.Errorf("the browser build was not fetched:\n%s", joined)
	}

	// Installed is installed: a second call touches nothing.
	before := len(*calls)
	if _, installed, err := EnsureCamofox(context.Background(), home, nil); err != nil || installed || len(*calls) != before {
		t.Errorf("second call: installed=%v err=%v calls=%d→%d", installed, err, before, len(*calls))
	}
}

func TestEnsureCamofoxLeavesLoadableBindingsAlone(t *testing.T) {
	home := t.TempDir()
	stubCommands(t, 22, true)
	if _, _, err := EnsureCamofox(context.Background(), home, nil); err != nil {
		t.Fatal(err)
	}
	for _, si := range standInList {
		data, _ := os.ReadFile(filepath.Join(CamofoxDir(home), "node_modules", si.file))
		if string(data) != "native" {
			t.Errorf("a loadable %s was replaced: %q", si.module, data)
		}
	}
}

// The stand-in for SQLite has to be what camoufox-js actually calls, on the
// Node the installer accepts: a database opened by path, prepare().all() and
// close().
func TestSQLiteStandInAnswersLikeBetterSQLite(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	if have, err := nodeVersionOf(context.Background(), node); err != nil || !versionAtLeast(have, nodeMinVersion) {
		t.Skipf("node %s is older than %s", have, nodeMinVersion)
	}
	dir := t.TempDir()
	var body string
	for _, si := range standInList {
		if si.module == "better-sqlite3" {
			body = si.body
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "standin.js"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `const Database = require("./standin.js");
const db = new Database(":memory:");
db.exec("create table t(vendor text, renderer text); insert into t values ('v1','r1'),('v2','r2')");
const rows = db.prepare("select distinct vendor, renderer from t where vendor > ?").all("v1");
db.close();
console.log(JSON.stringify(rows));`
	cmd := exec.Command(node, "-e", script)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), `[{"vendor":"v2","renderer":"r2"}]`) {
		t.Errorf("stand-in answered %q, %v", out, err)
	}
}

// The gateway provisions at startup, and takes the engine nobody reads any
// more with it.
func TestProvisionInBackgroundInstallsAndRetiresLightpanda(t *testing.T) {
	home := t.TempDir()
	old := filepath.Join(home, "engine", "lightpanda")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	called := make(chan string, 1)
	saved := provisionCamofox
	provisionCamofox = func(_ context.Context, h string, _ Progress) (string, bool, error) {
		called <- h
		return "server.js", true, nil
	}
	t.Cleanup(func() { provisionCamofox = saved })

	ProvisionInBackground(context.Background(), config.BrowserConfig{Enabled: true, Engine: "auto"}, home)
	select {
	case h := <-called:
		if h != home {
			t.Errorf("provisioned into %q, want %q", h, home)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway did not provision")
	}
	if executable(old) {
		t.Error("the Lightpanda binary was kept")
	}

	// Forced to Chromium, nothing is fetched.
	ProvisionInBackground(context.Background(), config.BrowserConfig{Enabled: true, Engine: "chromium"}, home)
	select {
	case <-called:
		t.Error("provisioned under browser.engine chromium")
	case <-time.After(200 * time.Millisecond):
	}
}

// nodeRelease serves a fake nodejs.org release: a tarball holding one node
// binary, and the checksum file that vouches for it.
func nodeRelease(t *testing.T, tamper bool) *httptest.Server {
	t.Helper()
	asset, err := nodeAsset()
	if err != nil {
		t.Skip(err)
	}
	var tarball strings.Builder
	gz := gzip.NewWriter(&tarball)
	tw := tar.NewWriter(gz)
	root := strings.TrimSuffix(strings.TrimSuffix(asset, ".tar.gz"), ".zip")
	bin := root + "/bin/node"
	if runtime.GOOS == "windows" {
		bin = root + "/node.exe"
	}
	body := []byte("#!/bin/sh\necho v22.9.9\n")
	_ = tw.WriteHeader(&tar.Header{Name: root + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: root + "/bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: bin, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256([]byte(tarball.String()))
	checksum := hex.EncodeToString(sum[:])
	if tamper {
		checksum = strings.Repeat("0", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "SHASUMS256.txt"):
			fmt.Fprintf(w, "deadbeef  something-else.tar.gz\n%s  %s\n", checksum, asset)
		case strings.HasSuffix(r.URL.Path, asset):
			_, _ = w.Write([]byte(tarball.String()))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	saved := nodeDist
	nodeDist = srv.URL + "/v%s/"
	t.Cleanup(func() { nodeDist = saved })
	return srv
}

func TestEnsureNodeDownloadsAndVerifiesTheRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release is a tarball")
	}
	home := t.TempDir()
	nodeRelease(t, false)
	stubCommands(t, 0, true) // nothing on PATH
	var progress []string
	node, err := ensureNode(context.Background(), home, func(f string, a ...any) { progress = append(progress, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	if node != provisionedNode(home) || !executable(node) {
		t.Errorf("node = %q, want the provisioned binary", node)
	}
	if !strings.Contains(strings.Join(progress, "\n"), "installed Node "+nodeVersion) {
		t.Errorf("progress:\n%s", strings.Join(progress, "\n"))
	}
	// Found next time without a download.
	if again, err := findNode(home); err != nil || again != node {
		t.Errorf("findNode = %q, %v", again, err)
	}
}

func TestEnsureNodeRefusesATamperedDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release is a tarball")
	}
	home := t.TempDir()
	nodeRelease(t, true)
	stubCommands(t, 0, true)
	_, err := ensureNode(context.Background(), home, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want the checksum refusal", err)
	}
	if executable(provisionedNode(home)) {
		t.Error("a tampered Node was installed")
	}
}

func TestFindNodeRefusesAnOldNode(t *testing.T) {
	stubCommands(t, 18, true)
	if _, err := findNode(t.TempDir()); err == nil {
		t.Error("Node 18 was accepted")
	}
	stubCommands(t, 22, true)
	if node, err := findNode(t.TempDir()); err != nil || node != "/usr/bin/node" {
		t.Errorf("findNode = %q, %v", node, err)
	}
}

func TestSafeJoinRefusesEscapes(t *testing.T) {
	dest := t.TempDir()
	if _, err := safeJoin(dest, "../outside"); err == nil {
		t.Error("an escaping entry was accepted")
	}
	if p, err := safeJoin(dest, "inside/file"); err != nil || p != filepath.Join(dest, "inside", "file") {
		t.Errorf("safeJoin = %q, %v", p, err)
	}
}

func TestNpmForSitsBesideNode(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	npm := filepath.Join(dir, "npm")
	for _, p := range []string{node, npm} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" && npmFor(node) != npm {
		t.Errorf("npmFor = %q, want %q", npmFor(node), npm)
	}
	if got := npmFor("/nowhere/node"); got != "npm" && got != "npm.cmd" {
		t.Errorf("npmFor without a sibling = %q", got)
	}
}
