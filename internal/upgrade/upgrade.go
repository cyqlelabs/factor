// Package upgrade keeps Factor current: it finds the newest published
// release for this machine and replaces the running binary with it.
//
// The release workflow publishes one binary per platform, named for its
// GOOS/GOARCH, plus a SHA256SUMS covering them all — so an upgrade is a
// download, a checksum, and a rename, with no package manager involved. Those
// names are fixed, which is what lets every URL here be built rather than
// looked up in GitHub's rate-limited API.
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
	// GitHub's own release pages, not its API. api.github.com answers 60
	// requests an hour per address and counts every one of them, a
	// conditional request it replies 304 to included — a budget shared by
	// everything behind that address, so a machine on an office NAT can find
	// it spent without having asked for anything. These pages are not on that
	// budget, and they answer the same two questions: /releases/latest
	// redirects to the newest tag, and /releases/download/<tag>/<name> serves
	// an asset — the very URL the API used to hand back.
	releasesURL = "https://github.com/" + repo + "/releases"
	// A lookup is a redirect and a few hundred bytes of checksums; the
	// budget above is the download's.
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
	Sum     string // the SHA256 the release publishes for Asset
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
	tag, err := latestTag(ctx)
	if err != nil {
		return Release{}, err
	}
	rel := Release{
		Version: tag,
		Asset:   AssetName(),
		URL:     releasesURL + "/download/" + tag + "/" + AssetName(),
		Notes:   releasesURL + "/tag/" + tag,
	}
	if rel.Sum, err = publishedSum(ctx, tag, rel.Asset); err != nil {
		return Release{}, err
	}
	saveReleaseCache(releaseCache{Checked: time.Now(), Release: rel})
	return rel, nil
}

// latestTag reads the newest tag out of the redirect /releases/latest answers
// with. It is a HEAD and the redirect is deliberately not followed: the tag is
// in the Location header, and the page behind it is a megabyte of HTML nobody
// here reads.
func latestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, releasesURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	client := httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("looking up the latest factor release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("looking up the latest factor release: %s", describeStatus(resp))
	}
	// ".../releases/tag/v0.4.0". A repository that has published nothing is
	// redirected to the release index instead, which names no tag.
	loc := resp.Header.Get("Location")
	_, tag, ok := strings.Cut(loc, "/releases/tag/")
	if !ok || tag == "" || strings.Contains(tag, "/") {
		return "", fmt.Errorf("the latest factor release carries no tag: GitHub pointed at %q", loc)
	}
	return tag, nil
}

// publishedSum returns the SHA256 the release publishes for this machine's
// binary. SHA256SUMS is the release's own index — every binary it published,
// by name, with the digest to check it against — so the one small fetch that
// answers "is there a build for this machine" also answers "what must it hash
// to", and a name missing from it is never downloaded at all.
func publishedSum(ctx context.Context, tag, asset string) (string, error) {
	resp, err := fetch(ctx, releasesURL+"/download/"+tag+"/SHA256SUMS")
	if err != nil {
		return "", fmt.Errorf("fetching the SHA256SUMS of release %s: %w", tag, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading the SHA256SUMS of release %s: %w", tag, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// "<sha>  <name>", with a leading * on the name in binary mode.
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("release %s publishes no %s build", tag, asset)
}

// releaseCheckFloor is how recent a stored answer has to be for the watcher's
// startup check to trust it instead of asking again. Every watcher opens with
// a check, so without a floor a machine that restarts all day asks GitHub on
// every start. An hour caps that at one lookup per window and still notices a
// release the day it lands.
const releaseCheckFloor = time.Hour

// latestNoOlderThan answers from the last stored check while it is younger
// than age, so restarts cost nothing. An explicit `factor upgrade` never comes
// through here: someone who asks is owed a fresh answer.
func latestNoOlderThan(ctx context.Context, age time.Duration) (Release, error) {
	if c, ok := loadReleaseCache(); ok && time.Since(c.Checked) < age {
		return c.Release, nil
	}
	return Latest(ctx)
}

// releaseCache is the last answer GitHub gave and when it gave it.
type releaseCache struct {
	Checked time.Time `json:"checked"`
	Release Release   `json:"release"`
}

func releaseCachePath() string { return filepath.Join(config.Home(), "release-check.json") }

// loadReleaseCache reports a usable cache: one holding a release resolved for
// the binary this machine runs. Anything else — no file, a rewritten home, a
// cache carried to another architecture, a clock that moved backwards, one
// written before a release carried its checksum — is no cache, and the check
// asks GitHub.
func loadReleaseCache() (releaseCache, bool) {
	raw, err := os.ReadFile(releaseCachePath())
	if err != nil {
		return releaseCache{}, false
	}
	var c releaseCache
	if err := json.Unmarshal(raw, &c); err != nil {
		return releaseCache{}, false
	}
	if c.Release.Version == "" || c.Release.Sum == "" || c.Release.Asset != AssetName() || c.Checked.After(time.Now()) {
		return releaseCache{}, false
	}
	return c, true
}

// saveReleaseCache is best effort: a cache that cannot be written costs one
// counted request per restart, which is not worth failing an upgrade over.
func saveReleaseCache(c releaseCache) {
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.WriteFile(releaseCachePath(), raw, 0o600); err != nil {
		slog.Debug("could not cache the release check", "error", err)
	}
}

// describeStatus says what a refusal actually was. A 403 or 429 on a public
// repository is almost never permissions: it is GitHub throttling the address,
// which everything behind that address shares. Saying "403 Forbidden" sends
// the reader looking for a credential that was never needed.
func describeStatus(resp *http.Response) string {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return resp.Status
	}
	msg := "GitHub is rate-limiting this address, which everything behind it shares"
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		msg += "; it clears at " + time.Now().Add(time.Duration(secs)*time.Second).Format("15:04")
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

	// Nothing unverified is ever swapped in, and the only release without a
	// checksum is one this code did not resolve.
	if rel.Sum == "" {
		return "", fmt.Errorf("release %s carries no published checksum", rel.Version)
	}

	// Staged beside the binary: the swap is a rename, and a rename only
	// works within one filesystem.
	staged := exe + ".new"
	if err := download(ctx, rel, staged, info.Mode().Perm()|0o100, progress); err != nil {
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

func download(ctx context.Context, rel Release, dest string, mode os.FileMode, progress Progress) error {
	resp, err := fetch(ctx, rel.URL)
	if err != nil {
		return fmt.Errorf("downloading factor %s: %w", rel.Version, err)
	}
	defer resp.Body.Close()
	// The size comes off the response rather than out of the release index,
	// which is what lets the lookup be a redirect and a checksum file.
	if resp.ContentLength > 0 {
		progress("downloading factor %s (%d MB)", rel.Version, resp.ContentLength>>20)
	} else {
		progress("downloading factor %s", rel.Version)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		// The common failure by far: the binary lives somewhere this user
		// cannot write, and the raw open error alone does not say so.
		return fmt.Errorf("staging the new binary beside the old one: %w", err)
	}
	defer f.Close()
	sum := sha256.New()
	src := io.TeeReader(&progressReader{r: resp.Body, total: resp.ContentLength, report: progress}, sum)
	if _, err := io.Copy(f, src); err != nil {
		return fmt.Errorf("downloading factor %s: %w", rel.Version, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != rel.Sum {
		return fmt.Errorf("the downloaded binary does not match the published checksum (got %s, want %s)", got, rel.Sum)
	}
	return nil
}

func fetch(ctx context.Context, url string) (*http.Response, error) {
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
		return nil, fmt.Errorf("%s", describeStatus(resp))
	}
	return resp, nil
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
	fresh := min(every, releaseCheckFloor)
	watchLoop(ctx, every, func() {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rel, err := latestNoOlderThan(callCtx, fresh)
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
