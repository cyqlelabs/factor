// Package upgrade keeps Factor current: it finds the newest published
// release for this machine and replaces the running binary with it.
//
// The release workflow publishes one binary per platform, named for its
// GOOS/GOARCH, plus a SHA256SUMS covering them all — so an upgrade is a
// download, a checksum, and a rename, with no package manager involved.
package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
)

const repo = "cyqlelabs/factor"

// DefaultCheckInterval is how often the gateway looks for a new release.
const DefaultCheckInterval = 24 * time.Hour

// Overridable seams so the whole path is testable without the network or a
// real binary to overwrite.
var (
	releaseAPI = "https://api.github.com/repos/" + repo + "/releases/latest"
	// A lookup is one small JSON GET; the budget above is the download's.
	lookupTimeout  = 30 * time.Second
	executablePath = os.Executable
	goos           = runtime.GOOS
	goarch         = runtime.GOARCH
)

// httpClient is built for each request rather than kept, and it is built from
// whatever http.DefaultTransport is at that moment. That is the whole point:
// `-p` / proxy.address installs the proxy's certificate authority there, and a
// client built when this package was loaded would have copied the transport
// that existed before main() had read the config — which is how a self-upgrade
// behind an intercepting proxy failed on GitHub's certificate while every
// other call Factor makes went through.
//
// Every request here is a rare one-off — a check a day, a download now and
// then — so a pooled connection buys nothing and costs everything: the one
// kept from the last lookup is the one the network quietly dropped in
// between, and an HTTP/2 stream reopened on it answers nothing while the
// download's fifteen minutes run out. A fresh HTTP/1.1 connection per
// request cannot wedge that way, and the dial and the wait for headers are
// bounded so only the body may take its time.
func httpClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	clone := transport.Clone()
	clone.Proxy = http.ProxyFromEnvironment
	clone.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	clone.TLSHandshakeTimeout = 15 * time.Second
	clone.ResponseHeaderTimeout = 30 * time.Second
	clone.DisableKeepAlives = true
	return &http.Client{Timeout: 15 * time.Minute, Transport: clone}
}

// Progress reports steps to whoever is watching (the CLI prints them).
type Progress func(format string, args ...any)

// Release is the newest published build for this machine.
type Release struct {
	Version string // the tag, e.g. "v0.4.0"
	Asset   string // the binary's file name
	URL     string // where to download it
	SumsURL string // the SHA256SUMS covering it
	Size    int64
	Notes   string // the release page
}

// AssetName is what the release workflow calls this machine's binary.
func AssetName() string {
	arch := goarch
	if goos == "linux" && arch == "arm" {
		arch = "armv7" // the workflow builds arm with GOARM=7
	}
	name := "factor-" + goos + "-" + arch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Latest returns the newest published release, resolved to the asset this
// machine can actually run.
func Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// An unauthenticated caller gets 60 requests an hour, counted per address,
	// and every machine behind one router shares that. A check runs at every
	// start — which on a box whose watchdog restarts the gateway is a check a
	// minute — so the answer is asked for conditionally: GitHub does not count
	// a 304 against the budget, and the release only changes on release day.
	cached, conditional := loadReleaseCache()
	if conditional {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("looking up the latest factor release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && conditional {
		return cached.Release, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("looking up the latest factor release: %s", describeStatus(resp))
	}
	var body struct {
		Tag    string `json:"tag_name"`
		HTML   string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("reading the factor release index: %w", err)
	}
	if body.Tag == "" {
		return Release{}, fmt.Errorf("the latest factor release carries no tag")
	}
	rel := Release{Version: body.Tag, Notes: body.HTML, Asset: AssetName()}
	for _, a := range body.Assets {
		switch a.Name {
		case rel.Asset:
			rel.URL, rel.Size = a.URL, a.Size
		case "SHA256SUMS":
			rel.SumsURL = a.URL
		}
	}
	if rel.URL == "" {
		return Release{}, fmt.Errorf("release %s publishes no %s build", rel.Version, rel.Asset)
	}
	saveReleaseCache(releaseCache{ETag: resp.Header.Get("ETag"), Release: rel})
	return rel, nil
}

// releaseCache is the last answer GitHub gave and the ETag identifying it, so
// the next check can ask whether anything changed instead of asking again.
type releaseCache struct {
	ETag    string  `json:"etag"`
	Release Release `json:"release"`
}

func releaseCachePath() string { return filepath.Join(config.Home(), "release-check.json") }

// loadReleaseCache reports a usable cache: one with an ETag, holding a release
// resolved for the binary this machine runs. Anything else — no file, a
// rewritten home, a cache carried to another architecture — is no cache, and
// the check falls back to an ordinary request.
func loadReleaseCache() (releaseCache, bool) {
	raw, err := os.ReadFile(releaseCachePath())
	if err != nil {
		return releaseCache{}, false
	}
	var c releaseCache
	if err := json.Unmarshal(raw, &c); err != nil {
		return releaseCache{}, false
	}
	if c.ETag == "" || c.Release.Version == "" || c.Release.Asset != AssetName() {
		return releaseCache{}, false
	}
	return c, true
}

// saveReleaseCache is best effort: a cache that cannot be written costs one
// counted request next time, which is not worth failing an upgrade over.
func saveReleaseCache(c releaseCache) {
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.WriteFile(releaseCachePath(), raw, 0o600); err != nil {
		slog.Debug("could not cache the release check", "error", err)
	}
}

// describeStatus says what a refusal actually was. A 403 from this endpoint is
// almost never permissions on a public repository: it is the hourly budget,
// spent by every unauthenticated caller sharing the address. Saying "403
// Forbidden" sends the reader looking for a credential that was never needed.
func describeStatus(resp *http.Response) string {
	limited := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests
	if !limited || resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return resp.Status
	}
	msg := "GitHub's API rate limit for this address is used up"
	if limit := resp.Header.Get("X-RateLimit-Limit"); limit != "" {
		msg += " (" + limit + " an hour, shared by everything behind it)"
	}
	if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		msg += "; it clears at " + time.Unix(secs, 0).Format("15:04")
	}
	return msg
}

// Newer reports whether want is a later release than have. A have that is
// not a release version — "dev", or `git describe` output from a working
// tree — counts as older: there is no released version it could be current
// at. A describe suffix is ignored, so a build four commits past v0.3.0 is
// current with v0.3.0 rather than being told to install what it already has.
func Newer(have, want string) bool {
	w, ok := parseVersion(want)
	if !ok {
		return false
	}
	h, ok := parseVersion(have)
	if !ok {
		return true
	}
	for i := range w {
		if w[i] != h[i] {
			return w[i] > h[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Apply replaces the running binary with rel, verifying its published
// checksum first, and returns the path it replaced. The new version takes
// effect on the next start: a running process keeps the code it has loaded.
func Apply(ctx context.Context, rel Release, progress Progress) (string, error) {
	if progress == nil {
		progress = func(string, ...any) {}
	}
	exe, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("finding the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved // replace the binary, not the symlink pointing at it
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "", err
	}

	want, err := checksum(ctx, rel)
	if err != nil {
		return "", err
	}

	// Staged beside the binary: the swap is a rename, and a rename only
	// works within one filesystem.
	staged := exe + ".new"
	progress("downloading factor %s (%d MB)", rel.Version, rel.Size>>20)
	if err := download(ctx, rel, staged, info.Mode().Perm()|0o100, want, progress); err != nil {
		_ = os.Remove(staged)
		return "", err
	}

	// Two renames rather than one: Windows cannot rename onto a running
	// executable, and moving the old binary aside first leaves every
	// platform something to put back when the swap fails.
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("replacing %s: %w", exe, err)
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Rename(old, exe)
		_ = os.Remove(staged)
		return "", fmt.Errorf("replacing %s: %w", exe, err)
	}
	_ = os.Remove(old) // Windows holds it until this process exits
	return exe, nil
}

// checksum returns the SHA256 the release publishes for this machine's
// binary. A release without one is not installed: swapping in an unverified
// binary is precisely what this must never do.
func checksum(ctx context.Context, rel Release) (string, error) {
	if rel.SumsURL == "" {
		return "", fmt.Errorf("release %s publishes no SHA256SUMS", rel.Version)
	}
	body, err := fetch(ctx, rel.SumsURL)
	if err != nil {
		return "", fmt.Errorf("fetching SHA256SUMS: %w", err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading SHA256SUMS: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// "<sha>  <name>", with a leading * on the name in binary mode.
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == rel.Asset {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("the SHA256SUMS of release %s does not cover %s", rel.Version, rel.Asset)
}

func download(ctx context.Context, rel Release, dest string, mode os.FileMode, want string, progress Progress) error {
	body, err := fetch(ctx, rel.URL)
	if err != nil {
		return fmt.Errorf("downloading factor %s: %w", rel.Version, err)
	}
	defer body.Close()
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		// The common failure by far: the binary lives somewhere this user
		// cannot write, and the raw open error alone does not say so.
		return fmt.Errorf("staging the new binary beside the old one: %w", err)
	}
	defer f.Close()
	sum := sha256.New()
	src := io.TeeReader(&progressReader{r: body, total: rel.Size, report: progress}, sum)
	if _, err := io.Copy(f, src); err != nil {
		return fmt.Errorf("downloading factor %s: %w", rel.Version, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("the downloaded binary does not match the published checksum (got %s, want %s)", got, want)
	}
	return nil
}

func fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return resp.Body, nil
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

// Watch polls for a newer release and reports each one once. It only ever
// reports: installing is an explicit `factor upgrade` or a tool call, never
// something a daemon does to itself underneath a live conversation.
func Watch(ctx context.Context, every time.Duration, current string, notify func(Release)) {
	if every <= 0 {
		every = DefaultCheckInterval
	}
	told := ""
	watchLoop(ctx, every, func() {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rel, err := Latest(callCtx)
		cancel()
		if err != nil {
			slogDebug("release check failed", err)
			return
		}
		if rel.Version == told || !Newer(current, rel.Version) {
			return
		}
		told = rel.Version
		notify(rel)
	})
}

// watchLoop runs check now and then every interval, until ctx ends. Both
// watchers start with a check: news that arrived while Factor was down is
// still news.
func watchLoop(ctx context.Context, every time.Duration, check func()) {
	if every <= 0 {
		every = DefaultCheckInterval
	}
	check()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

func slogDebug(msg string, err error) { slog.Debug(msg, "error", err) }
