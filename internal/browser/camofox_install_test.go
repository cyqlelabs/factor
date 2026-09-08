//go:build !nobrowser

package browser

import (
	"archive/tar"
	"archive/zip"
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

// zipWith builds an archive holding the named entries, so the unpacker can
// be tested on what a Windows Node release looks like without one.
func zipWith(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, body := range entries {
		if strings.HasSuffix(name, "/") {
			if _, err := w.Create(name); err != nil {
				t.Fatal(err)
			}
			continue
		}
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.SetMode(0o755)
		e, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnzipUnpacksAndKeepsModes(t *testing.T) {
	archive := zipWith(t, map[string]string{
		"node-v22/":         "",
		"node-v22/node.exe": "binary",
		"node-v22/lib/x.js": "module",
	})
	dest := t.TempDir()
	if err := unzip(archive, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "node-v22", "node.exe"))
	if err != nil || string(data) != "binary" {
		t.Fatalf("node.exe = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "node-v22", "lib", "x.js")); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "node-v22", "node.exe"))
	if err != nil || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		t.Errorf("the executable bit was lost: %v %v", info.Mode(), err)
	}
	if root, err := singleDir(dest); err != nil || filepath.Base(root) != "node-v22" {
		t.Errorf("singleDir = %q, %v", root, err)
	}
}

// An archive entry naming a path outside the destination is refused rather
// than written: a release is a download, and a download is not trusted to
// choose where it lands.
func TestUnzipRefusesAnEscapingEntry(t *testing.T) {
	archive := zipWith(t, map[string]string{"../escaped.txt": "no"})
	dest := t.TempDir()
	if err := unzip(archive, dest); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("err = %v, want the escape refused", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.txt")); err == nil {
		t.Error("the entry was written outside the destination")
	}
}

// tarWith builds a gzipped tarball from a list of entries, including the
// symlinks a real Node release carries.
type tarEntry struct {
	name, body, link string
	dir              bool
}

func tarWith(t *testing.T, entries []tarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755}
		switch {
		case e.dir:
			hdr.Typeflag = tar.TypeDir
		case e.link != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.link
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUntarGzUnpacksFilesDirsAndLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege there")
	}
	archive := tarWith(t, []tarEntry{
		{name: "node-v22/", dir: true},
		{name: "node-v22/bin/", dir: true},
		{name: "node-v22/bin/node", body: "binary"},
		{name: "node-v22/bin/npm", link: "../lib/npm.js"},
	})
	dest := t.TempDir()
	if err := untarGz(archive, dest); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "node-v22", "bin", "node")); err != nil || string(data) != "binary" {
		t.Fatalf("node = %q, %v", data, err)
	}
	link, err := os.Readlink(filepath.Join(dest, "node-v22", "bin", "npm"))
	if err != nil || link != "../lib/npm.js" {
		t.Errorf("npm link = %q, %v", link, err)
	}
	// Unpacking twice is how a retried install behaves; the link must not
	// stop it.
	if err := untarGz(archive, dest); err != nil {
		t.Errorf("second unpack: %v", err)
	}
}

func TestUntarGzRefusesAnEscapingEntry(t *testing.T) {
	archive := tarWith(t, []tarEntry{{name: "../escaped", body: "no"}})
	if err := untarGz(archive, t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("err = %v", err)
	}
}

func TestUntarGzRejectsSomethingThatIsNotAnArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.tar.gz")
	if err := os.WriteFile(path, []byte("this is not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := untarGz(path, t.TempDir()); err == nil {
		t.Error("a plain file was accepted as a tarball")
	}
	if err := untarGz(filepath.Join(t.TempDir(), "missing.tar.gz"), t.TempDir()); err == nil {
		t.Error("a missing archive was accepted")
	}
	if err := unzip(filepath.Join(t.TempDir(), "missing.zip"), t.TempDir()); err == nil {
		t.Error("a missing zip was accepted")
	}
}

// The checksum file lists every asset of a release; the one for this machine
// has to be found in it, and a release that lists none is a refusal.
func TestNodeChecksumFindsTheAssetAndReportsAMissingOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "empty"):
			fmt.Fprint(w, "deadbeef  some-other-build.tar.gz\n")
		case strings.Contains(r.URL.Path, "broken"):
			w.WriteHeader(http.StatusNotFound)
		default:
			fmt.Fprintf(w, "  \nabc123  wanted.tar.gz\ndeadbeef  other.tar.gz\n")
		}
	}))
	defer srv.Close()

	got, err := nodeChecksum(context.Background(), srv.URL+"/SHASUMS256.txt", "wanted.tar.gz")
	if err != nil || got != "abc123" {
		t.Errorf("checksum = %q, %v", got, err)
	}
	if _, err := nodeChecksum(context.Background(), srv.URL+"/empty", "wanted.tar.gz"); err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Errorf("err = %v, want the missing asset reported", err)
	}
	if _, err := nodeChecksum(context.Background(), srv.URL+"/broken", "wanted.tar.gz"); err == nil {
		t.Error("an unreadable checksum file was accepted")
	}
	if _, err := nodeChecksum(context.Background(), "http://127.0.0.1:1/x", "a"); err == nil {
		t.Error("an unreachable host was accepted")
	}
}

func TestFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fileSHA256(path)
	if err != nil || got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256 = %q, %v", got, err)
	}
	if _, err := fileSHA256(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file was hashed")
	}
}

// A release without a build for this machine is reported before anything is
// downloaded.
func TestEnsureNodeReportsAReleaseWithoutThisMachinesBuild(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "deadbeef  node-v1.2.3-nowhere-x64.tar.gz\n")
	}))
	defer srv.Close()
	saved := nodeDist
	nodeDist = srv.URL + "/v%s/"
	t.Cleanup(func() { nodeDist = saved })
	stubCommands(t, 0, true)

	if _, err := ensureNode(context.Background(), t.TempDir(), func(string, ...any) {}); err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("err = %v", err)
	}
}

// A download the server refuses is reported rather than written as an empty
// archive.
func TestEnsureNodeReportsAFailedDownload(t *testing.T) {
	asset, err := nodeAsset()
	if err != nil {
		t.Skip(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHASUMS256.txt") {
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), asset)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	saved := nodeDist
	nodeDist = srv.URL + "/v%s/"
	t.Cleanup(func() { nodeDist = saved })
	stubCommands(t, 0, true)

	home := t.TempDir()
	if _, err := ensureNode(context.Background(), home, func(string, ...any) {}); err == nil || !strings.Contains(err.Error(), "downloading Node") {
		t.Fatalf("err = %v", err)
	}
	if executable(provisionedNode(home)) {
		t.Error("a refused download left a Node behind")
	}
}

// npm failing is the install failing: nothing is reported as installed, and
// what npm said comes back with it.
func TestEnsureCamofoxReportsAFailedInstall(t *testing.T) {
	stubCommands(t, 22, true)
	saved := runCmdEnv
	runCmdEnv = func(_ context.Context, argv []string, _ []string, _ string) (string, error) {
		if len(argv) > 1 && argv[1] == "install" {
			return "npm error code E404\nnot found", errors.New("exit status 1")
		}
		return "", nil
	}
	t.Cleanup(func() { runCmdEnv = saved })

	home := t.TempDir()
	server, installed, err := EnsureCamofox(context.Background(), home, nil)
	if err == nil || !strings.Contains(err.Error(), "E404") {
		t.Fatalf("err = %v, want what npm said", err)
	}
	if server != "" || installed {
		t.Errorf("a failed install reported %q installed=%v", server, installed)
	}
}

// npm can report success and lay down nothing, which is a failure that has
// to be caught here rather than at the first page.
func TestEnsureCamofoxCatchesAnEmptyInstall(t *testing.T) {
	stubCommands(t, 22, true)
	saved := runCmdEnv
	runCmdEnv = func(context.Context, []string, []string, string) (string, error) { return "", nil }
	t.Cleanup(func() { runCmdEnv = saved })

	_, _, err := EnsureCamofox(context.Background(), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("err = %v", err)
	}
}

// The browser build is what the engine cannot start without, so a fetch that
// leaves nothing behind fails the install.
func TestEnsureCamofoxCatchesAFetchThatFetchedNothing(t *testing.T) {
	stubCommands(t, 22, true)
	saved := runCmdEnv
	inner := runCmdEnv
	runCmdEnv = func(ctx context.Context, argv, env []string, dir string) (string, error) {
		if len(argv) > 2 && argv[2] == "fetch" {
			return "network unreachable", errors.New("exit status 1")
		}
		return inner(ctx, argv, env, dir)
	}
	t.Cleanup(func() { runCmdEnv = saved })

	_, _, err := EnsureCamofox(context.Background(), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "Camoufox build failed") {
		t.Fatalf("err = %v", err)
	}
}

// Every platform Factor builds for has to name an archive nodejs.org
// publishes, and name it exactly: this is checked from one machine because
// a wrong name is a 404 on somebody else's.
func TestNodeAssetNamesEveryPlatform(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "node-v" + nodeVersion + "-linux-x64.tar.gz"},
		{"linux", "arm64", "node-v" + nodeVersion + "-linux-arm64.tar.gz"},
		{"darwin", "amd64", "node-v" + nodeVersion + "-darwin-x64.tar.gz"},
		{"darwin", "arm64", "node-v" + nodeVersion + "-darwin-arm64.tar.gz"},
		{"windows", "amd64", "node-v" + nodeVersion + "-win-x64.zip"},
		{"windows", "arm64", "node-v" + nodeVersion + "-win-arm64.zip"},
	} {
		got, err := nodeAssetFor(c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("%s/%s = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
	for _, c := range []struct{ goos, goarch string }{
		{"linux", "386"}, {"linux", "riscv64"}, {"freebsd", "amd64"}, {"plan9", "arm64"},
	} {
		if got, err := nodeAssetFor(c.goos, c.goarch); err == nil {
			t.Errorf("%s/%s named %q, want it refused", c.goos, c.goarch, got)
		}
	}
}

// The interpreter and the browser both live where the platform puts them.
func TestProvisionedPathsFollowThePlatform(t *testing.T) {
	home := filepath.Join("h", ".factor")
	node := provisionedNode(home)
	wantNode := filepath.Join(home, "engine", "node", "bin", "node")
	if runtime.GOOS == "windows" {
		wantNode = filepath.Join(home, "engine", "node", "node.exe")
	}
	if node != wantNode {
		t.Errorf("node = %q, want %q", node, wantNode)
	}
	if got := camofoxServer(CamofoxDir(home)); got != filepath.Join(home, "engine", "camofox", "node_modules", "@askjo", "camofox-browser", "server.js") {
		t.Errorf("server = %q", got)
	}
	if got := camoufoxDir(CamofoxDir(home)); got != filepath.Join(home, "engine", "camofox", "camoufox") {
		t.Errorf("camoufox dir = %q", got)
	}
}

// A node whose --version says nothing usable is not a node to run the
// engine on.
func TestNodeVersionOfRejectsNonsense(t *testing.T) {
	saved := runCmd
	runCmd = func(_ context.Context, argv []string) (string, error) {
		if strings.Contains(argv[0], "broken") {
			return "", errors.New("exit status 127")
		}
		return "not a version at all\n", nil
	}
	t.Cleanup(func() { runCmd = saved })
	if _, err := nodeVersionOf(context.Background(), "/usr/bin/node"); err == nil || !strings.Contains(err.Error(), "not a version") {
		t.Errorf("err = %v, want what node printed", err)
	}
	if _, err := nodeVersionOf(context.Background(), "/usr/bin/broken"); err == nil {
		t.Error("a node that would not run was accepted")
	}
}

// A stand-in is written only where the module exists: a package version that
// does not carry it is left alone rather than gaining a file.
func TestStandInsSkipModulesThatAreNotThere(t *testing.T) {
	dir := t.TempDir()
	saved := runCmdEnv
	runCmdEnv = func(context.Context, []string, []string, string) (string, error) {
		t.Error("probed a module that is not installed")
		return "", nil
	}
	t.Cleanup(func() { runCmdEnv = saved })
	if err := standIns(context.Background(), "node", dir, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	for _, si := range standInList {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", si.file)); err == nil {
			t.Errorf("wrote a stand-in for %s, which is not installed", si.module)
		}
	}
}

// A load failure that is not the C library is reported rather than papered
// over: a stand-in would hide a real break in the package.
func TestStandInsReportOtherLoadFailures(t *testing.T) {
	dir := t.TempDir()
	for _, si := range standInList {
		path := filepath.Join(dir, "node_modules", si.file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("native"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	saved := runCmdEnv
	runCmdEnv = func(context.Context, []string, []string, string) (string, error) {
		return "SyntaxError: something else entirely", errors.New("exit status 1")
	}
	t.Cleanup(func() { runCmdEnv = saved })
	err := standIns(context.Background(), "node", dir, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "something else") {
		t.Fatalf("err = %v, want the real failure reported", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "node_modules", standInList[0].file))
	if string(data) != "native" {
		t.Error("the module was replaced despite an unexplained failure")
	}
}
