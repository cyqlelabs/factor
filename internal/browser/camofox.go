//go:build !nobrowser

package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// camofox drives the headless engine: a Node server wrapping Camoufox, a
// Firefox build whose fingerprint is spoofed in C++ before any page script
// runs, which is what gets past the sites that turn a headless Chromium
// away. It answers the same eleven tools as the Chromium session, through
// its REST API instead of DevTools: pages come back as accessibility
// snapshots with element refs, which is both what the model needs to act
// and a tenth of the HTML.
//
// The server is spawned on first use and left running; it launches Firefox
// lazily and shuts it down when idle, so a Factor that is not browsing costs
// the box a sleeping Node process. One already answering on the port is
// adopted rather than raced.
type camofox struct {
	cfg       config.BrowserConfig
	home      string
	workspace string
	guard     *tools.PathGuard
	base      string
	client    *http.Client

	mu           sync.Mutex
	cmd          *exec.Cmd
	tab          string // the tab the tools act on; "" until the first navigation
	installTried bool
}

// camofoxUser is the session every tab is opened under. The server isolates
// cookies per user; Factor is one user.
const camofoxUser = "factor"

// camofoxStart bounds how long the server may take to answer /health after
// it is spawned. Node starts in a second; the first launch also fetches an
// ad-blocking addon.
var camofoxStart = 30 * time.Second

// provisionCamofox is EnsureCamofox behind a seam, so a test can exercise
// the provisioning path without reaching the network.
var provisionCamofox = EnsureCamofox

func newCamofox(cfg config.BrowserConfig, home, workspace string, guard *tools.PathGuard) *camofox {
	if cfg.Camofox.Port <= 0 {
		cfg.Camofox.Port = 9377
	}
	return &camofox{
		cfg:       cfg,
		home:      home,
		workspace: workspace,
		guard:     guard,
		base:      fmt.Sprintf("http://127.0.0.1:%d", cfg.Camofox.Port),
		client:    &http.Client{Timeout: 90 * time.Second},
	}
}

// dir is where the package lives.
func (c *camofox) dir() string {
	if c.cfg.Camofox.Dir != "" {
		return c.cfg.Camofox.Dir
	}
	return CamofoxDir(c.home)
}

// healthy reports whether a server answers on the port.
func (c *camofox) healthy() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(c.base + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ensure returns once a server answers, spawning one when nothing does. A
// port something else holds is reported rather than fought over: the child
// would die on bind and the next call would spawn another.
func (c *camofox) ensure(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.healthy() {
		return nil
	}
	if c.cmd != nil {
		c.stopLocked()
	}
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(c.base, "http://"), time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("port %d is held by something that is not Camofox; free it or move browser.camofox.port", c.cfg.Camofox.Port)
	}
	server, err := c.server(ctx)
	if err != nil {
		return err
	}
	node, err := c.node()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.home, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(c.home, "camofox.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(node, server)
	cmd.Dir = filepath.Dir(server)
	cmd.Env = c.env()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("starting Camofox: %w", err)
	}
	logFile.Close()
	c.cmd = cmd
	slog.Info("browser: started Camofox", "pid", cmd.Process.Pid, "port", c.cfg.Camofox.Port)
	deadline := time.Now().Add(camofoxStart)
	for time.Now().Before(deadline) {
		if c.healthy() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.stopLocked()
	return fmt.Errorf("camofox did not answer on port %d within %s; its log is %s", c.cfg.Camofox.Port, camofoxStart, filepath.Join(c.home, "camofox.log"))
}

// server resolves the package's entry point, installing the package when
// the machine has none. Handing the user a command to run instead is not an
// option here for the same reason it is not for the Chromium engine: on the
// boxes that most need a browser that answer never becomes one.
func (c *camofox) server(ctx context.Context) (string, error) {
	path := camofoxServer(c.dir())
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if c.cfg.Camofox.Dir != "" {
		return "", fmt.Errorf("browser.camofox.dir %q holds no Camofox install (%s is missing)", c.cfg.Camofox.Dir, path)
	}
	if c.installTried {
		return "", fmt.Errorf("camofox is not installed at %s and installing it failed earlier in this run; re-run `factor init`", c.dir())
	}
	c.installTried = true
	slog.Info("browser: Camofox is not installed, provisioning it")
	installCtx, cancel := context.WithTimeout(ctx, InstallTimeout)
	defer cancel()
	path, installed, err := provisionCamofox(installCtx, c.home, func(format string, args ...any) {
		slog.Info("browser: " + fmt.Sprintf(format, args...))
	})
	if err != nil {
		return "", fmt.Errorf("camofox is not installed and provisioning it failed: %w", err)
	}
	if installed {
		slog.Info("browser: provisioned Camofox", "server", path)
	}
	return path, nil
}

// node resolves the interpreter the server runs on.
func (c *camofox) node() (string, error) {
	path, err := findNode(c.home)
	if err != nil {
		return "", fmt.Errorf("camofox needs Node %s or newer: %w", nodeMinVersion, err)
	}
	return path, nil
}

// env is the server's environment. Telemetry is switched off for the reason
// it is on the memory and speech sidecars: the crash reporter posts to the
// project's collector by default, and nothing this process runs phones
// anyone unasked. Everything the server writes is kept under Factor's own
// directories, so an install and its profiles leave with it.
func (c *camofox) env() []string {
	state := filepath.Join(c.cfg.UserDataDir, "camofox")
	if c.cfg.UserDataDir == "" {
		state = filepath.Join(c.home, "browser", "camofox")
	}
	env := os.Environ()
	set := func(k, v string) { env = append(env, k+"="+v) }
	set("CAMOFOX_PORT", strconv.Itoa(c.cfg.Camofox.Port))
	set("CAMOFOX_BIND_HOST", "127.0.0.1")
	set("CAMOFOX_CRASH_REPORT_ENABLED", "false")
	set("CAMOUFOX_INSTALL_DIR", camoufoxDir(c.dir()))
	set("CAMOFOX_PROFILE_DIR", filepath.Join(state, "profiles"))
	set("CAMOFOX_COOKIES_DIR", filepath.Join(state, "cookies"))
	set("CAMOFOX_TRACES_DIR", filepath.Join(state, "traces"))
	set("CAMOFOX_UPLOADS_DIR", c.workspace)
	return env
}

func (c *camofox) stopLocked() {
	if c.cmd == nil {
		return
	}
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	c.cmd = nil
}

// Close stops a server this process started. One it adopted stays up: it
// belongs to whoever started it.
func (c *camofox) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

// --- the REST client ---------------------------------------------------------

// camofoxError is what the server says when it refuses. The recovery hint is
// the part worth passing on: "stale_refs" says to read the page again, which
// is exactly what the model should do.
type camofoxError struct {
	Message string `json:"error"`
	Code    string `json:"code"`
}

func (e *camofoxError) Error() string { return e.Message }

// call sends one request and decodes a JSON reply into out (nil to discard).
func (c *camofox) call(ctx context.Context, method, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("camofox did not answer: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var ce camofoxError
		if json.Unmarshal(data, &ce) == nil && ce.Message != "" {
			return &ce
		}
		return fmt.Errorf("camofox answered %s: %s", resp.Status, firstLine(string(data)))
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

// withUser adds the session the server keys everything by.
func withUser(fields map[string]any) map[string]any {
	fields["userId"] = camofoxUser
	return fields
}

// currentTab is the tab the tools act on, or an error naming the tool that
// opens one.
func (c *camofox) currentTab() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tab == "" {
		return "", fmt.Errorf("no page is open; browser_navigate first")
	}
	return c.tab, nil
}

// lostTab reports whether the server no longer knows the tab, which happens
// when Firefox was shut down idle: the tab is gone and the next navigation
// opens a fresh one.
func lostTab(err error) bool {
	var ce *camofoxError
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Code == "tab_not_found" || strings.Contains(strings.ToLower(ce.Message), "tab not found")
}

var refPattern = regexp.MustCompile(`^e\d+$`)

// targetOf splits a tool's target into the field the server wants: a ref
// from the last snapshot, or a CSS selector.
func targetOf(target string) map[string]any {
	if refPattern.MatchString(target) {
		return map[string]any{"ref": target}
	}
	return map[string]any{"selector": target}
}

// --- what the tools call -----------------------------------------------------

func (c *camofox) navigate(ctx context.Context, url string) *tools.Result {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return tools.Errorf("Camofox opens http and https pages only; read a local file with read_file")
	}
	if err := c.ensure(ctx); err != nil {
		return tools.Errorf("navigate failed: %v", err)
	}
	tab, _ := c.currentTab()
	if tab != "" {
		var r struct {
			URL string `json:"url"`
		}
		err := c.call(ctx, http.MethodPost, "/tabs/"+tab+"/navigate", withUser(map[string]any{"url": url}), &r)
		if err == nil {
			return c.snapshot(ctx, "", 0, 0)
		}
		if !lostTab(err) {
			return tools.Errorf("navigate failed: %v", err)
		}
	}
	var created struct {
		TabID string `json:"tabId"`
	}
	if err := c.call(ctx, http.MethodPost, "/tabs", withUser(map[string]any{"sessionKey": camofoxUser, "url": url}), &created); err != nil {
		return tools.Errorf("navigate failed: %v", err)
	}
	c.mu.Lock()
	c.tab = created.TabID
	c.mu.Unlock()
	return c.snapshot(ctx, "", 0, 0)
}

// camofoxSnapshot is the server's reading of a page.
type camofoxSnapshot struct {
	URL        string `json:"url"`
	Snapshot   string `json:"snapshot"`
	Refs       int    `json:"refsCount"`
	Truncated  bool   `json:"truncated"`
	TotalChars int    `json:"totalChars"`
}

// snapshot reads the current page. filter keeps only the lines that mention
// the text; limit caps how many elements with refs are listed; offset
// continues a snapshot the server cut for length.
func (c *camofox) snapshot(ctx context.Context, filter string, limit, offset int) *tools.Result {
	tab, err := c.currentTab()
	if err != nil {
		return tools.Errorf("%v", err)
	}
	path := "/tabs/" + tab + "/snapshot?userId=" + camofoxUser
	if offset > 0 {
		path += "&offset=" + strconv.Itoa(offset)
	}
	var snap camofoxSnapshot
	if err := c.call(ctx, http.MethodGet, path, nil, &snap); err != nil {
		if lostTab(err) {
			c.mu.Lock()
			c.tab = ""
			c.mu.Unlock()
			return tools.Errorf("the page is gone (the browser was shut down idle); browser_navigate again")
		}
		return tools.Errorf("read failed: %v", err)
	}
	title := c.title(ctx, tab)
	return tools.Text(formatSnapshot(title, snap, filter, limit, offset))
}

// title asks the page for its title, which the snapshot does not carry. A
// failure is not worth failing the read over.
func (c *camofox) title(ctx context.Context, tab string) string {
	var r struct {
		Result any `json:"result"`
	}
	if err := c.call(ctx, http.MethodPost, "/tabs/"+tab+"/evaluate", withUser(map[string]any{"expression": "document.title"}), &r); err != nil {
		return ""
	}
	s, _ := r.Result.(string)
	return s
}

// formatSnapshot renders a reading the way the Chromium engine renders its
// own: title and URL first, then the page, then what was withheld and how
// to get it — a cut the model cannot see reads as an empty page.
func formatSnapshot(title string, snap camofoxSnapshot, filter string, limit, offset int) string {
	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "%s — %s\n\n", title, snap.URL)
	} else {
		fmt.Fprintf(&b, "%s\n\n", snap.URL)
	}
	lines := strings.Split(strings.TrimRight(snap.Snapshot, "\n"), "\n")
	needle := strings.ToLower(strings.TrimSpace(filter))
	shown, refs, withheld := 0, 0, 0
	for _, line := range lines {
		if needle != "" && !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		if limit > 0 && strings.Contains(line, "[e") {
			refs++
			if refs > limit {
				withheld++
				continue
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
		shown++
	}
	if shown == 0 {
		if needle != "" {
			fmt.Fprintf(&b, "(nothing on the page mentions %q)\n", filter)
		} else {
			b.WriteString("(the page has no readable content yet; browser_read again once it settles)\n")
		}
	}
	if withheld > 0 {
		fmt.Fprintf(&b, "\n(%d more elements not listed — raise limit or pass filter)\n", withheld)
	}
	if snap.Truncated {
		end := offset + len(snap.Snapshot)
		fmt.Fprintf(&b, "\n(showing characters %d–%d of %d — browser_read with offset=%d reads on)\n", offset, end, snap.TotalChars, end)
	}
	return b.String()
}

func (c *camofox) click(ctx context.Context, target string) *tools.Result {
	return c.act(ctx, "click", "/click", targetOf(target))
}

func (c *camofox) fill(ctx context.Context, target, text string, submit bool) *tools.Result {
	fields := targetOf(target)
	fields["text"] = text
	fields["submit"] = submit
	return c.act(ctx, "fill", "/type", fields)
}

func (c *camofox) keys(ctx context.Context, chord string) *tools.Result {
	return c.act(ctx, "keys", "/press", map[string]any{"key": chord})
}

func (c *camofox) back(ctx context.Context) *tools.Result {
	return c.act(ctx, "back", "/back", map[string]any{})
}

// scroll moves the page. The server scrolls by a distance in a direction;
// the ends of the page are reached through the page itself.
func (c *camofox) scroll(ctx context.Context, to, filter string) *tools.Result {
	tab, err := c.currentTab()
	if err != nil {
		return tools.Errorf("%v", err)
	}
	switch to {
	case "top", "bottom":
		expr := "window.scrollTo(0, 0)"
		if to == "bottom" {
			expr = "window.scrollTo(0, document.documentElement.scrollHeight)"
		}
		if err := c.call(ctx, http.MethodPost, "/tabs/"+tab+"/evaluate", withUser(map[string]any{"expression": expr}), nil); err != nil {
			return tools.Errorf("scroll failed: %v", err)
		}
	default:
		direction := "down"
		if to == "up" {
			direction = "up"
		}
		if err := c.call(ctx, http.MethodPost, "/tabs/"+tab+"/scroll", withUser(map[string]any{"direction": direction, "amount": 800}), nil); err != nil {
			return tools.Errorf("scroll failed: %v", err)
		}
	}
	// Content that loads on the way down needs a beat to arrive.
	time.Sleep(900 * time.Millisecond)
	return c.snapshot(ctx, filter, 0, 0)
}

// act performs one page action and reads the page after it.
func (c *camofox) act(ctx context.Context, what, path string, fields map[string]any) *tools.Result {
	tab, err := c.currentTab()
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if err := c.call(ctx, http.MethodPost, "/tabs/"+tab+path, withUser(fields), nil); err != nil {
		return tools.Errorf("%s failed: %v", what, err)
	}
	return c.snapshot(ctx, "", 0, 0)
}

func (c *camofox) eval(ctx context.Context, expression string) *tools.Result {
	tab, err := c.currentTab()
	if err != nil {
		return tools.Errorf("%v", err)
	}
	var r struct {
		Result any `json:"result"`
	}
	if err := c.call(ctx, http.MethodPost, "/tabs/"+tab+"/evaluate", withUser(map[string]any{"expression": expression}), &r); err != nil {
		return tools.Errorf("eval failed: %v", err)
	}
	out, err := json.Marshal(r.Result)
	if err != nil {
		return tools.Errorf("eval result is not JSON: %v", err)
	}
	return tools.Text(string(out))
}

func (c *camofox) screenshot(ctx context.Context) *tools.Result {
	tab, err := c.currentTab()
	if err != nil {
		return tools.Errorf("%v", err)
	}
	var png []byte
	if err := c.call(ctx, http.MethodGet, "/tabs/"+tab+"/screenshot?userId="+camofoxUser, nil, &png); err != nil {
		return tools.Errorf("screenshot failed: %v", err)
	}
	dir := filepath.Join(c.workspace, "screenshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.Errorf("%v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("shot-%d.png", time.Now().Unix()))
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return tools.Errorf("%v", err)
	}
	return &tools.Result{ForLLM: "Saved screenshot to " + path, ForUser: path}
}

// upload attaches a file. The server only serves files under its uploads
// directory, which is the workspace, so the path is checked by the guard and
// then handed over relative to it.
func (c *camofox) upload(ctx context.Context, path, target string) *tools.Result {
	if path == "" {
		return tools.Errorf("path is required")
	}
	resolved := path
	if c.guard != nil {
		var err error
		if resolved, err = c.guard.CheckRead(path); err != nil {
			return tools.Errorf("%v", err)
		}
	}
	rel, err := filepath.Rel(c.workspace, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		return tools.Errorf("Camofox attaches files from the workspace only; copy %s into it first", path)
	}
	if target == "" {
		target = "input[type=file]"
	}
	fields := targetOf(target)
	fields["path"] = filepath.ToSlash(rel)
	return c.act(ctx, "upload", "/upload", fields)
}

// camofoxTab is one open tab as the server lists it.
type camofoxTab struct {
	ID    string `json:"tabId"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func (c *camofox) listTabs(ctx context.Context) ([]camofoxTab, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	var r struct {
		Tabs []camofoxTab `json:"tabs"`
	}
	if err := c.call(ctx, http.MethodGet, "/tabs?userId="+camofoxUser, nil, &r); err != nil {
		return nil, err
	}
	sort.SliceStable(r.Tabs, func(i, j int) bool { return r.Tabs[i].ID < r.Tabs[j].ID })
	return r.Tabs, nil
}

func (c *camofox) tabs(ctx context.Context, action, target, url string) *tools.Result {
	switch action {
	case "", "list":
		open, err := c.listTabs(ctx)
		if err != nil {
			return tools.Errorf("listing tabs failed: %v", err)
		}
		if len(open) == 0 {
			return tools.Text("No tabs are open.")
		}
		return tools.Text(formatTabs(c.tabInfos(open)))
	case "switch":
		open, err := c.listTabs(ctx)
		if err != nil {
			return tools.Errorf("listing tabs failed: %v", err)
		}
		idx, found := c.resolveTab(open, target)
		if !found {
			return tools.Errorf("no tab matches %q", target)
		}
		c.mu.Lock()
		c.tab = open[idx].ID
		c.mu.Unlock()
		return c.snapshot(ctx, "", 0, 0)
	case "open":
		if url == "" {
			return tools.Errorf("url is required to open a tab")
		}
		c.mu.Lock()
		c.tab = ""
		c.mu.Unlock()
		return c.navigate(ctx, url)
	case "close":
		open, err := c.listTabs(ctx)
		if err != nil {
			return tools.Errorf("listing tabs failed: %v", err)
		}
		id := ""
		if target == "" {
			id, _ = c.currentTab()
		} else if idx, found := c.resolveTab(open, target); found {
			id = open[idx].ID
		}
		if id == "" {
			return tools.Errorf("no tab matches %q", target)
		}
		if err := c.call(ctx, http.MethodDelete, "/tabs/"+id+"?userId="+camofoxUser, nil, nil); err != nil {
			return tools.Errorf("closing the tab failed: %v", err)
		}
		c.mu.Lock()
		if c.tab == id {
			c.tab = ""
		}
		c.mu.Unlock()
		return tools.Text("Closed the tab.")
	}
	return tools.Errorf("unknown action %q (list, switch, open, close)", action)
}

// tabInfos renders the server's list in the shape the Chromium engine uses,
// so the model reads both the same way.
func (c *camofox) tabInfos(open []camofoxTab) []tabInfo {
	current, _ := c.currentTab()
	out := make([]tabInfo, len(open))
	for i, t := range open {
		out[i] = tabInfo{Index: i + 1, Title: t.Title, URL: t.URL, Current: t.ID == current}
	}
	return out
}

// resolveTab finds a tab by its number in the listing, or by text in its
// title or URL.
func (c *camofox) resolveTab(open []camofoxTab, query string) (int, bool) {
	if n, err := strconv.Atoi(strings.TrimSpace(query)); err == nil {
		if n >= 1 && n <= len(open) {
			return n - 1, true
		}
		return 0, false
	}
	needle := strings.ToLower(query)
	for i, t := range open {
		if strings.Contains(strings.ToLower(t.Title), needle) || strings.Contains(strings.ToLower(t.URL), needle) {
			return i, true
		}
	}
	return 0, false
}
