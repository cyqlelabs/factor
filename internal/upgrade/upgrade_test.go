package upgrade

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		have, want string
		newer      bool
	}{
		{"v0.3.0", "v0.4.0", true},
		{"v0.3.0", "v0.3.1", true},
		{"v0.3.0", "v1.0.0", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.4.0", "v0.3.0", false},
		{"0.3.0", "v0.4.0", true},             // the leading v is optional either side
		{"v0.3.0-4-gabc123", "v0.3.0", false}, // a build past the tag is not behind it
		{"v0.3.0-4-gabc123", "v0.4.0", true},
		{"v0.3.0-dirty", "v0.3.0", false},
		{"dev", "v0.4.0", true},     // no released version to be current at
		{"unknown", "v0.4.0", true}, //
		{"", "v0.4.0", true},
		{"v0.3.0", "dev", false}, // an unparseable candidate is never an upgrade
		{"v0.3.0", "", false},    //
		{"v0.3", "v0.3.1", true}, // short versions pad with zeros
		{"v0.3.0", "v0.3.0.1", false},
	}
	for _, c := range cases {
		if got := Newer(c.have, c.want); got != c.newer {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.have, c.want, got, c.newer)
		}
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct{ osName, arch, want string }{
		{"linux", "amd64", "factor-linux-amd64"},
		{"linux", "386", "factor-linux-386"},
		{"linux", "arm64", "factor-linux-arm64"},
		{"linux", "arm", "factor-linux-armv7"},
		{"darwin", "arm64", "factor-darwin-arm64"},
		{"windows", "amd64", "factor-windows-amd64.exe"},
	}
	for _, c := range cases {
		setPlatform(t, c.osName, c.arch)
		if got := AssetName(); got != c.want {
			t.Errorf("AssetName() on %s/%s = %q, want %q", c.osName, c.arch, got, c.want)
		}
	}
}

// release serves a GitHub-shaped release plus its assets.
type release struct {
	tag    string
	binary []byte
	assets []string // asset names to publish; nil means the usual pair
	sums   string   // SHA256SUMS body; empty means one generated for binary
}

// start publishes the release and points releaseAPI at it, returning the
// base URL and the mux so a test can add a misbehaving asset of its own.
func (r release) start(t *testing.T) (string, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	// The listener exists before Start, so the download URLs the release
	// index hands out can be built without racing the serving goroutine.
	base := "http://" + srv.Listener.Addr().String()

	names := r.assets
	if names == nil {
		names = []string{AssetName(), "SHA256SUMS"}
	}
	sums := r.sums
	if sums == "" {
		sums = fmt.Sprintf("%s  %s\n", sha256hex(r.binary), AssetName())
	}

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		// GitHub tags every answer, which is what the next check asks against.
		w.Header().Set("ETag", `W/"`+r.tag+`"`)
		var assets []string
		for _, n := range names {
			size := len(r.binary)
			if n == "SHA256SUMS" {
				size = len(sums)
			}
			assets = append(assets, fmt.Sprintf(`{"name":%q,"browser_download_url":"%s/dl/%s","size":%d}`, n, base, n, size))
		}
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://example.test/%s","assets":[%s]}`,
			r.tag, r.tag, strings.Join(assets, ","))
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, sums)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(r.binary)
	})

	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv("FACTOR_HOME", t.TempDir()) // the release check caches its ETag under it
	prev := releaseAPI
	releaseAPI = base + "/releases/latest"
	t.Cleanup(func() { releaseAPI = prev })
	return base, mux
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func setPlatform(t *testing.T, osName, arch string) {
	t.Helper()
	prevOS, prevArch := goos, goarch
	goos, goarch = osName, arch
	t.Cleanup(func() { goos, goarch = prevOS, prevArch })
}

// stageBinary points executablePath at a throwaway file standing in for the
// running binary, and returns its path.
func stageBinary(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "factor")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })
	return path
}

func TestLatest(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("binary")}.start(t)

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v0.4.0" || rel.Asset != "factor-linux-amd64" {
		t.Fatalf("got %+v", rel)
	}
	if rel.URL == "" || rel.SumsURL == "" || rel.Size != 6 {
		t.Fatalf("release not fully resolved: %+v", rel)
	}
	if rel.Notes != "https://example.test/v0.4.0" {
		t.Errorf("notes = %q", rel.Notes)
	}
}

func TestLatestNoBuildForPlatform(t *testing.T) {
	setPlatform(t, "plan9", "mips")
	release{tag: "v0.4.0", binary: []byte("binary"), assets: []string{"factor-linux-amd64", "SHA256SUMS"}}.start(t)

	_, err := Latest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "factor-plan9-mips") {
		t.Fatalf("err = %v, want it to name the missing build", err)
	}
}

func TestLatestFailures(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/notfound":
			http.Error(w, "nope", http.StatusNotFound)
		case "/garbage":
			fmt.Fprint(w, "{{{")
		default:
			fmt.Fprint(w, `{"tag_name":"","assets":[]}`)
		}
	}))
	defer srv.Close()

	for _, c := range []struct{ path, want string }{
		{"/notfound", "404"},
		{"/garbage", "reading the factor release index"},
		{"/untagged", "no tag"},
	} {
		prev := releaseAPI
		releaseAPI = srv.URL + c.path
		_, err := Latest(context.Background())
		releaseAPI = prev
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.path, err, c.want)
		}
	}

	prev := releaseAPI
	releaseAPI = "http://127.0.0.1:0/nothing-listening"
	_, err := Latest(context.Background())
	releaseAPI = prev
	if err == nil {
		t.Error("an unreachable API should be an error")
	}
}

func TestApplyReplacesTheBinary(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)
	exe := stageBinary(t, "the old build")

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	path, err := Apply(context.Background(), rel, func(f string, a ...any) {
		steps = append(steps, fmt.Sprintf(f, a...))
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != exe {
		t.Errorf("replaced %q, want %q", path, exe)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the new build" {
		t.Errorf("binary content = %q", got)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	// Windows carries executability in the extension, not a mode bit.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the installed binary is not executable (%v)", info.Mode())
	}
	for _, leftover := range []string{exe + ".new", exe + ".old"} {
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("%s was left behind", filepath.Base(leftover))
		}
	}
	if len(steps) == 0 || !strings.Contains(steps[0], "v0.4.0") {
		t.Errorf("progress = %v, want it to name the version", steps)
	}
}

func TestApplyRejectsAMismatchedChecksum(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build"),
		sums: "0000000000000000000000000000000000000000000000000000000000000000  " + AssetName() + "\n"}.start(t)
	exe := stageBinary(t, "the old build")

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want a checksum complaint", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the old build" {
		t.Errorf("the running binary was replaced anyway: %q", got)
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Error("the rejected download was left behind")
	}
}

func TestApplyChecksumFailures(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	stageBinary(t, "the old build")

	t.Run("no sums published", func(t *testing.T) {
		_, err := Apply(context.Background(), Release{Version: "v0.4.0", Asset: AssetName()}, nil)
		if err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("sums omit this asset", func(t *testing.T) {
		release{tag: "v0.4.0", binary: []byte("x"), sums: "abc  factor-openbsd-riscv\n"}.start(t)
		rel, err := Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "does not cover") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("sums unreachable", func(t *testing.T) {
		_, err := Apply(context.Background(), Release{Version: "v0.4.0", Asset: AssetName(),
			SumsURL: "http://127.0.0.1:0/nothing"}, nil)
		if err == nil || !strings.Contains(err.Error(), "fetching SHA256SUMS") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestApplyDownloadFailure(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	base, mux := release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)
	exe := stageBinary(t, "the old build")

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rel.URL = base + "/missing/asset" // resolves, then 404s
	mux.HandleFunc("/missing/asset", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})
	if _, err := Apply(context.Background(), rel, nil); err == nil || !strings.Contains(err.Error(), "downloading factor") {
		t.Fatalf("err = %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "the old build" {
		t.Errorf("binary content = %q", got)
	}
}

func TestApplyWithoutARunningBinary(t *testing.T) {
	prev := executablePath
	executablePath = func() (string, error) { return "", fmt.Errorf("no /proc") }
	defer func() { executablePath = prev }()

	if _, err := Apply(context.Background(), Release{Version: "v0.4.0"}, nil); err == nil ||
		!strings.Contains(err.Error(), "finding the running binary") {
		t.Fatalf("err = %v", err)
	}

	executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "gone"), nil }
	if _, err := Apply(context.Background(), Release{Version: "v0.4.0"}, nil); err == nil {
		t.Fatal("a missing binary should be an error")
	}
}

func TestApplyReportsDownloadProgress(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: make([]byte, 64*1024)}.start(t)
	stageBinary(t, "old")

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	if _, err := Apply(context.Background(), rel, func(f string, a ...any) {
		steps = append(steps, fmt.Sprintf(f, a...))
	}); err != nil {
		t.Fatal(err)
	}
	var percents int
	for _, s := range steps {
		if strings.HasPrefix(s, "downloaded ") {
			percents++
		}
	}
	if percents == 0 {
		t.Errorf("no download progress reported: %v", steps)
	}
}

func TestWatchReportsEachVersionOnce(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("x")}.start(t)

	seen := watching(t, 10*time.Millisecond, "v0.3.0")

	select {
	case rel := <-seen:
		if rel.Version != "v0.4.0" {
			t.Fatalf("reported %q", rel.Version)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no report")
	}
	select {
	case rel := <-seen:
		t.Fatalf("reported %q twice", rel.Version)
	case <-time.After(100 * time.Millisecond):
	}
}

// watching runs Watch for the duration of one test. Its goroutine is joined
// before the test's other cleanups restore the seams it reads, which a bare
// cancel would not guarantee.
func watching(t *testing.T, every time.Duration, current string) <-chan Release {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan Release, 4)
	done := make(chan struct{})
	go func() {
		Watch(ctx, every, current, func(r Release) { seen <- r })
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return seen
}

func TestWatchStaysQuietWhenCurrent(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("x")}.start(t)

	seen := watching(t, 0, "v0.4.0") // 0 falls back to the default interval

	select {
	case rel := <-seen:
		t.Fatalf("reported %q while already on it", rel.Version)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatchSurvivesAFailedCheck(t *testing.T) {
	prev := releaseAPI
	releaseAPI = "http://127.0.0.1:0/nothing-listening"
	t.Cleanup(func() { releaseAPI = prev })

	seen := watching(t, 10*time.Millisecond, "v0.3.0")
	select {
	case rel := <-seen:
		t.Fatalf("reported %q from a failed check", rel.Version)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestApplyIntoAnUnwritableDirectory(t *testing.T) {
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("the new build")}.start(t)

	dir := t.TempDir()
	exe := filepath.Join(dir, "factor")
	if err := os.WriteFile(exe, []byte("the old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Windows has no mode bit that stops a write into a directory, so the
	// staging path is blocked by putting a directory where the file must go.
	if runtime.GOOS == "windows" {
		if err := os.Mkdir(exe+".new", 0o700); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
	prev := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = prev })

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), rel, nil)
	if err == nil || !strings.Contains(err.Error(), "staging the new binary") {
		t.Fatalf("err = %v, want it to name what could not be written", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "the old build" {
		t.Errorf("binary content = %q", got)
	}
}

// A lookup must give up rather than hang. GitHub can accept the connection
// and then answer nothing at all — a connection the network dropped while it
// sat idle looks exactly like this — and a turn that asked Factor to upgrade
// itself would otherwise sit silent for the whole download budget.
func TestLatestGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	prevAPI, prevTimeout := releaseAPI, lookupTimeout
	releaseAPI, lookupTimeout = srv.URL+"/releases/latest", 100*time.Millisecond
	t.Cleanup(func() { releaseAPI, lookupTimeout = prevAPI, prevTimeout })

	done := make(chan error, 1)
	go func() {
		_, err := Latest(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from a server that never answers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Latest hung on a server that never answers")
	}
}

// An intercepting proxy (mitmproxy, Burp) is trusted by putting its authority
// on http.DefaultTransport, which main() does after reading the config — long
// after this package's variables are initialised. A client built at init
// carries the system roots alone, which made self-upgrade the one call that
// died on GitHub's certificate while every other call Factor makes went
// through the same proxy happily.
func TestTheUpgradeClientTrustsWhatTheProxyInstalled(t *testing.T) {
	setPlatform(t, "linux", "amd64")

	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","html_url":"https://example.test/v9.9.9","assets":[`+
			`{"name":%q,"browser_download_url":"%s/dl/binary","size":6},`+
			`{"name":"SHA256SUMS","browser_download_url":"%s/dl/SHA256SUMS","size":6}]}`,
			AssetName(), srv.URL, srv.URL)
	})
	prev := releaseAPI
	releaseAPI = srv.URL + "/releases/latest"
	t.Cleanup(func() { releaseAPI = prev })

	// Exactly what proxy.Use does to this process: the extra authority is
	// added to the transport everything else clones.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	original := http.DefaultTransport
	clone := original.(*http.Transport).Clone()
	clone.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	http.DefaultTransport = clone
	t.Cleanup(func() { http.DefaultTransport = original })

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatalf("looking up a release through a trusted intercepting proxy: %v", err)
	}
	if rel.Version != "v9.9.9" {
		t.Errorf("release = %+v", rel)
	}
}

// GitHub gives an unauthenticated caller 60 requests an hour per address, and
// a machine that checks at every start burns them on an answer it already has.
// The second check must ask conditionally and be told nothing changed.
func TestLatestAsksConditionallyOnceItHasAnAnswer(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	setPlatform(t, "linux", "amd64")
	base, mux := release{tag: "v0.4.0", binary: []byte("binary")}.start(t)
	_ = base

	var conditional []string
	mux.HandleFunc("/conditional/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		conditional = append(conditional, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `W/"tag-v0.4.0"`)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprintf(w, `{"tag_name":"v0.4.0","html_url":"https://example.test/v0.4.0","assets":[
			{"name":"factor-linux-amd64","browser_download_url":"%s/dl/factor-linux-amd64","size":6},
			{"name":"SHA256SUMS","browser_download_url":"%s/dl/SHA256SUMS","size":9}]}`, base, base)
	})
	prev := releaseAPI
	releaseAPI = base + "/conditional/releases/latest"
	t.Cleanup(func() { releaseAPI = prev })

	first, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Latest(context.Background())
	if err != nil {
		t.Fatalf("the conditional check failed: %v", err)
	}
	if second != first {
		t.Errorf("second = %+v, want the cached %+v", second, first)
	}
	if len(conditional) != 2 || conditional[0] != "" || conditional[1] != `W/"tag-v0.4.0"` {
		t.Errorf("If-None-Match sent = %q, want the second request to carry the ETag", conditional)
	}
}

// A cache written for another machine's binary is no cache: asking
// conditionally against it would answer 304 and hand back a release with a
// download URL this platform cannot run.
func TestLatestIgnoresACacheFromAnotherPlatform(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	setPlatform(t, "linux", "amd64")
	release{tag: "v0.4.0", binary: []byte("binary")}.start(t)
	if _, err := Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadReleaseCache(); !ok {
		t.Fatal("the first check stored no cache")
	}
	setPlatform(t, "darwin", "arm64")
	if _, ok := loadReleaseCache(); ok {
		t.Error("a cache resolved for another platform was accepted")
	}
}

// "403 Forbidden" reads as a credential problem on a public repository. It is
// the hourly budget, and what the reader needs is when it comes back.
func TestLatestNamesTheRateLimit(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	reset := time.Now().Add(13 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	prev := releaseAPI
	releaseAPI = srv.URL + "/releases/latest"
	t.Cleanup(func() { releaseAPI = prev })

	_, err := Latest(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"rate limit", "60 an hour", "clears at " + reset.Format("15:04")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// A 403 that is not the budget keeps its own words.
func TestLatestKeepsAnOrdinaryForbidden(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	prev := releaseAPI
	releaseAPI = srv.URL + "/releases/latest"
	t.Cleanup(func() { releaseAPI = prev })

	_, err := Latest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the status kept", err)
	}
}
