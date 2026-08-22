package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- looksLikeHTML ---

func TestLooksLikeHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty string", "", false},
		{"plain prose", "just some words about <angle> brackets", false},
		{"json", `{"tag": "html"}`, false},
		{"html element", "<html><body>hi</body></html>", true},
		{"uppercase html element", "<HTML>", true},
		{"doctype", "<!DOCTYPE html>\n<body>x</body>", true},
		{"doctype in mixed case", "<!DocType Html>", true},
		{"leading whitespace then html", "\n\n   <html>", true},
		{"xml that is not html", `<?xml version="1.0"?><rss><channel/></rss>`, false},
		{"marker beyond the 512 byte sniff window", strings.Repeat(" ", 600) + "<html>", false},
		{"marker just inside the sniff window", strings.Repeat(" ", 400) + "<html>", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeHTML(c.in); got != c.want {
				t.Errorf("looksLikeHTML = %v, want %v", got, c.want)
			}
		})
	}
}

// --- web_fetch ---

func textServer(t *testing.T, contentType, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWebFetchReturnsNonHTMLVerbatim(t *testing.T) {
	body := `{"ok": true, "note": "no markup here"}`
	srv := textServer(t, "application/json", body, http.StatusOK)
	res := (&webFetchTool{client: srv.Client()}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if res.ForLLM != body {
		t.Errorf("content = %q, want the body untouched", res.ForLLM)
	}
}

func TestWebFetchSniffsHTMLDespiteContentType(t *testing.T) {
	body := `<!doctype html><html><head><title>Sniffed</title></head><body><p>Body text.</p></body></html>`
	srv := textServer(t, "text/plain", body, http.StatusOK)
	res := (&webFetchTool{client: srv.Client()}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "Sniffed") || !strings.Contains(res.ForLLM, "Body text.") {
		t.Errorf("content = %q, want extracted text", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "<p>") {
		t.Errorf("markup leaked into the extracted text: %q", res.ForLLM)
	}
}

func TestWebFetchHTMLWithoutATitle(t *testing.T) {
	srv := textServer(t, "text/html", `<html><body><p>Only body.</p></body></html>`, http.StatusOK)
	res := (&webFetchTool{client: srv.Client()}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if res.IsError || res.ForLLM != "Only body." {
		t.Errorf("res = %+v, want the bare body text with no leading blank lines", res)
	}
}

func TestWebFetchHonorsMaxChars(t *testing.T) {
	srv := textServer(t, "text/plain", strings.Repeat("y", 500), http.StatusOK)
	res := (&webFetchTool{client: srv.Client()}).Execute(context.Background(), map[string]any{
		"url": srv.URL, "max_chars": 100.0,
	})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if want := strings.Repeat("y", 100) + "\n... [truncated]"; res.ForLLM != want {
		t.Errorf("content = %q, want it cut at max_chars with a truncation notice", res.ForLLM)
	}
}

func TestWebFetchMaxCharsLargerThanBodyIsANoOp(t *testing.T) {
	srv := textServer(t, "text/plain", "short", http.StatusOK)
	res := (&webFetchTool{client: srv.Client()}).Execute(context.Background(), map[string]any{
		"url": srv.URL, "max_chars": 10000.0,
	})
	if res.IsError || res.ForLLM != "short" {
		t.Errorf("res = %+v", res)
	}
}

func TestWebFetchReportsHTTPErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable} {
		srv := textServer(t, "text/plain", "upstream is unhappy", status)
		res := (&webFetchTool{client: srv.Client()}).Execute(context.Background(), map[string]any{"url": srv.URL})
		if !res.IsError || !strings.Contains(res.ForLLM, fmt.Sprintf("HTTP %d from", status)) {
			t.Errorf("status %d = %+v, want an HTTP error result", status, res)
		}
		if strings.Contains(res.ForLLM, "upstream is unhappy") {
			t.Errorf("status %d leaked the error body into the model context", status)
		}
	}
}

func TestWebFetchFollowsRedirects(t *testing.T) {
	final := textServer(t, "text/plain", "final body", http.StatusOK)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	res := (&webFetchTool{client: redirector.Client()}).Execute(context.Background(), map[string]any{"url": redirector.URL})
	if res.IsError || res.ForLLM != "final body" {
		t.Errorf("res = %+v, want the redirect target's body", res)
	}
}

// truncatedBodyServer promises more bytes than it delivers, then hangs up —
// the shape of a connection dropped mid-response.
func truncatedBodyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprint(buf, "HTTP/1.1 200 OK\r\nContent-Length: 4096\r\nContent-Type: text/plain\r\n\r\nshort")
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWebFetchBodyReadFailure(t *testing.T) {
	srv := truncatedBodyServer(t)
	res := (&webFetchTool{client: srv.Client()}).Execute(context.Background(), map[string]any{"url": srv.URL})
	if !res.IsError || !strings.Contains(res.ForLLM, "read failed") {
		t.Errorf("res = %+v, want a body read failure rather than a partial page", res)
	}
}

func TestWebFetchTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	client, addr := srv.Client(), srv.URL
	srv.Close() // nothing is listening any more

	res := (&webFetchTool{client: client}).Execute(context.Background(), map[string]any{"url": addr})
	if !res.IsError || !strings.Contains(res.ForLLM, "fetch failed") {
		t.Errorf("res = %+v, want a transport failure", res)
	}
}

func TestWebFetchRejectsNonHTTPURLs(t *testing.T) {
	tool := &webFetchTool{client: http.DefaultClient}
	for _, bad := range []string{"", "file:///etc/passwd", "ftp://host/f", "://nope", "data:text/plain,hi", "relative/path"} {
		res := tool.Execute(context.Background(), map[string]any{"url": bad})
		if !res.IsError || !strings.Contains(res.ForLLM, "invalid url") {
			t.Errorf("url %q = %+v, want a rejection", bad, res)
		}
	}
}

// --- web_search ---

func ddgResults(n int) string {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<div class="result"><a class="result__a" href="https://e%d.example/">Title %d</a>`+
			`<a class="result__snippet" href="#">Snippet %d.</a></div>`, i, i, i)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func TestWebSearchDefaultAndClampedCounts(t *testing.T) {
	srv := textServer(t, "text/html", ddgResults(12), http.StatusOK)
	tool := &webSearchTool{client: srv.Client(), searchURL: srv.URL}
	cases := []struct {
		name string
		args map[string]any
		want int
	}{
		{"default is five", map[string]any{"query": "q"}, 5},
		{"explicit count", map[string]any{"query": "q", "count": 3.0}, 3},
		{"count is clamped to ten", map[string]any{"query": "q", "count": 99.0}, 10},
		{"zero is clamped up to one", map[string]any{"query": "q", "count": 0.0}, 1},
		{"negative is clamped up to one", map[string]any{"query": "q", "count": -4.0}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := tool.Execute(context.Background(), c.args)
			if res.IsError {
				t.Fatalf("res = %+v", res)
			}
			if got := strings.Count(res.ForLLM, "https://e"); got != c.want {
				t.Errorf("returned %d results, want %d\n%s", got, c.want, res.ForLLM)
			}
		})
	}
}

func TestWebSearchNoResults(t *testing.T) {
	srv := textServer(t, "text/html", "<html><body><p>nothing matched</p></body></html>", http.StatusOK)
	tool := &webSearchTool{client: srv.Client(), searchURL: srv.URL}
	res := tool.Execute(context.Background(), map[string]any{"query": "obscure"})
	if res.IsError || res.ForLLM != "No results found." {
		t.Errorf("res = %+v, want a clean empty-result message", res)
	}
}

func TestWebSearchNon200(t *testing.T) {
	srv := textServer(t, "text/html", ddgResults(2), http.StatusTooManyRequests)
	tool := &webSearchTool{client: srv.Client(), searchURL: srv.URL}
	res := tool.Execute(context.Background(), map[string]any{"query": "q"})
	if !res.IsError || !strings.Contains(res.ForLLM, "search returned HTTP 429") {
		t.Errorf("res = %+v", res)
	}
}

func TestWebSearchBodyReadFailure(t *testing.T) {
	srv := truncatedBodyServer(t)
	tool := &webSearchTool{client: srv.Client(), searchURL: srv.URL}
	res := tool.Execute(context.Background(), map[string]any{"query": "q"})
	if !res.IsError || !strings.Contains(res.ForLLM, "read failed") {
		t.Errorf("res = %+v, want a body read failure", res)
	}
}

func TestWebSearchUnbuildableEndpoint(t *testing.T) {
	tool := &webSearchTool{client: http.DefaultClient, searchURL: ":"}
	res := tool.Execute(context.Background(), map[string]any{"query": "q"})
	if !res.IsError {
		t.Errorf("res = %+v, want an error for an unparseable endpoint", res)
	}
}

func TestWebSearchTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	client, addr := srv.Client(), srv.URL
	srv.Close()

	tool := &webSearchTool{client: client, searchURL: addr}
	res := tool.Execute(context.Background(), map[string]any{"query": "q"})
	if !res.IsError || !strings.Contains(res.ForLLM, "search failed") {
		t.Errorf("res = %+v", res)
	}
}

// --- duckduckgo parsing ---

func TestParseDuckDuckGoIgnoresUnclassedAnchors(t *testing.T) {
	src := `<html><body>
	<a href="https://ads.example/">Sponsored</a>
	<a class="result__snippet" href="#">Orphan snippet before any result.</a>
	<div class="result"><a class="result__a" href="https://real.example/">Real</a>
	<a class="result__snippet" href="#">First snippet.</a>
	<a class="result__snippet" href="#">Second snippet for the same result.</a></div>
	</body></html>`

	results := parseDuckDuckGo(src)
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly the classed result", results)
	}
	if results[0].title != "Real" || results[0].href != "https://real.example/" {
		t.Errorf("result = %+v", results[0])
	}
	if results[0].snippet != "First snippet." {
		t.Errorf("snippet = %q, want the first snippet to win", results[0].snippet)
	}
}

func TestParseDuckDuckGoEmptyInput(t *testing.T) {
	if results := parseDuckDuckGo(""); len(results) != 0 {
		t.Errorf("results = %+v, want none", results)
	}
}

func TestCleanDDGHref(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"decodes the uddg redirect", "//duckduckgo.com/l/?uddg=https%3A%2F%2Fa.example%2Fp&rut=x", "https://a.example/p"},
		{"passes direct links through", "https://direct.example.org/x", "https://direct.example.org/x"},
		{"redirect without uddg keeps the absolutized link", "//duckduckgo.com/l/?rut=abc", "https://duckduckgo.com/l/?rut=abc"},
		{"unparseable href is returned as-is", "https://%zz", "https://%zz"},
		{"empty href", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanDDGHref(c.in); got != c.want {
				t.Errorf("cleanDDGHref(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// --- pkg_install ---

func alwaysMissing(string) (string, error) {
	return "", errors.New("executable file not found in $PATH")
}
func alwaysFound(bin string) (string, error) { return "/usr/bin/" + bin, nil }

func refusingRunner(t *testing.T) func(context.Context, []string) (string, error) {
	t.Helper()
	return func(_ context.Context, argv []string) (string, error) {
		t.Errorf("runner must not be invoked, but got %v", argv)
		return "", nil
	}
}

func TestPkgInstallResolveErrors(t *testing.T) {
	cases := []struct {
		name     string
		manager  string
		lookPath func(string) (string, error)
		want     string
	}{
		{"unsupported manager", "brew", alwaysFound, `unsupported manager "brew"`},
		{"named manager not installed", "npm", alwaysMissing, "npm is not installed on this system"},
		{"probe name differs from manager name", "apt", alwaysMissing, "apt-get is not installed on this system"},
		{"auto finds nothing", "auto", alwaysMissing, "no supported system package manager found"},
		{"empty manager defaults to auto", "", alwaysMissing, "no supported system package manager found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := &PkgInstallTool{lookPath: c.lookPath, euid: func() int { return 0 }, runner: refusingRunner(t)}
			res := tool.Execute(context.Background(), map[string]any{
				"packages": []any{"htop"}, "manager": c.manager,
			})
			if !res.IsError || !strings.Contains(res.ForLLM, c.want) {
				t.Errorf("result = %+v, want it to contain %q", res, c.want)
			}
		})
	}
}

func TestPkgInstallAutoPrefersTheFirstAvailableManager(t *testing.T) {
	var captured []string
	tool := &PkgInstallTool{
		lookPath: func(bin string) (string, error) {
			if bin == "dnf" || bin == "pacman" {
				return "/usr/bin/" + bin, nil
			}
			return alwaysMissing(bin)
		},
		euid: func() int { return 0 },
		runner: func(_ context.Context, argv []string) (string, error) {
			captured = argv
			return "done", nil
		},
	}
	res := tool.Execute(context.Background(), map[string]any{"packages": []any{"htop"}})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if captured[0] != "dnf" {
		t.Errorf("argv = %v, want dnf (earlier in the probe order than pacman)", captured)
	}
}

func TestPkgInstallRequiresANonEmptyPackageList(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"no packages key", map[string]any{}},
		{"empty list", map[string]any{"packages": []any{}}},
		{"wrong type", map[string]any{"packages": "htop"}},
		{"nil", map[string]any{"packages": nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := &PkgInstallTool{lookPath: alwaysFound, euid: func() int { return 0 }, runner: refusingRunner(t)}
			res := tool.Execute(context.Background(), c.args)
			if !res.IsError || !strings.Contains(res.ForLLM, "non-empty list of names") {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestPkgInstallSudoUnavailable(t *testing.T) {
	tool := &PkgInstallTool{
		lookPath: func(bin string) (string, error) {
			if bin == "apt-get" {
				return "/usr/bin/apt-get", nil
			}
			return alwaysMissing(bin)
		},
		euid:   func() int { return 1000 },
		runner: refusingRunner(t),
	}
	res := tool.Execute(context.Background(), map[string]any{"packages": []any{"htop"}})
	if !res.IsError || !strings.Contains(res.ForLLM, "needs root and sudo is unavailable") {
		t.Errorf("result = %+v", res)
	}
}

func TestPkgInstallSurfacesInteractiveSudoPrompt(t *testing.T) {
	tool := &PkgInstallTool{
		lookPath: alwaysFound,
		euid:     func() int { return 1000 },
		runner: func(context.Context, []string) (string, error) {
			return "sudo: a password is required\n", errors.New("exit status 1")
		},
	}
	res := tool.Execute(context.Background(), map[string]any{"packages": []any{"htop"}, "manager": "apt"})
	if !res.IsError {
		t.Fatalf("res = %+v, want an error", res)
	}
	if !strings.Contains(res.ForLLM, "needs an interactive sudo password") {
		t.Errorf("result = %+v, want the interactive-sudo hint", res)
	}
	if !strings.Contains(res.ForLLM, "apt-get install -y htop") {
		t.Errorf("result = %+v, want a runnable command for the user", res)
	}
}

func TestPkgInstallFailureKeepsTheTailOfTheOutput(t *testing.T) {
	const head, tailMarker = "HEAD-MARKER", "TAIL-MARKER"
	tool := &PkgInstallTool{
		lookPath: alwaysFound,
		euid:     func() int { return 0 },
		runner: func(context.Context, []string) (string, error) {
			return head + strings.Repeat("e", 9*1024) + tailMarker, errors.New("exit status 100")
		},
	}
	res := tool.Execute(context.Background(), map[string]any{"packages": []any{"htop"}, "manager": "apt"})
	if !res.IsError || !strings.Contains(res.ForLLM, "install failed via apt") {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.ForLLM, tailMarker) {
		t.Error("the tail of a long failure log was dropped; that is where the real error lives")
	}
	if strings.Contains(res.ForLLM, head) {
		t.Error("output above the 8KB cap was not trimmed")
	}
}

func TestPkgInstallSuccessTailsLongOutput(t *testing.T) {
	const head, tailMarker = "HEAD-MARKER", "TAIL-MARKER"
	tool := &PkgInstallTool{
		lookPath: alwaysFound,
		euid:     func() int { return 0 },
		runner: func(context.Context, []string) (string, error) {
			return head + strings.Repeat("o", 3000) + tailMarker, nil
		},
	}
	res := tool.Execute(context.Background(), map[string]any{"packages": []any{"a", "b"}, "manager": "pip"})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.ForLLM, "Installed a, b via pip") {
		t.Errorf("result = %+v, want a summary naming every package", res)
	}
	if !strings.Contains(res.ForLLM, "…") || strings.Contains(res.ForLLM, head) {
		t.Errorf("long success output was not tailed: %q", res.ForLLM[:min(len(res.ForLLM), 120)])
	}
	if !strings.Contains(res.ForLLM, tailMarker) {
		t.Error("the tail of the success log was dropped")
	}
}

func TestTail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"shorter than the limit", "abc", 10, "abc"},
		{"exactly the limit", "abcde", 5, "abcde"},
		{"one over the limit", "abcdef", 5, "…bcdef"},
		{"well over the limit", "abcdef", 3, "…def"},
		{"zero keeps nothing", "abc", 0, "…"},
		{"empty input", "", 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tail(c.in, c.n); got != c.want {
				t.Errorf("tail(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func TestNewPkgInstallToolWiring(t *testing.T) {
	tool := NewPkgInstallTool()
	if tool.lookPath == nil || tool.runner == nil || tool.euid == nil {
		t.Fatal("NewPkgInstallTool left a dependency nil")
	}
	if got := tool.euid(); got != os.Geteuid() {
		t.Errorf("euid() = %d, want the real %d", got, os.Geteuid())
	}
	if _, err := tool.lookPath(shellExe); err != nil {
		t.Errorf("lookPath is not wired to PATH lookup: %v", err)
	}

	// Exercise the real runner on inert shell commands — never a real install.
	out, err := tool.runner(context.Background(), winArgv("printf hello; printf oops >&2", "(echo hello)& (echo oops)1>&2"))
	if err != nil {
		t.Fatalf("runner returned an error for a successful command: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "oops") {
		t.Errorf("runner output = %q, want stdout and stderr combined", out)
	}
	if _, err := tool.runner(context.Background(), winArgv("exit 7", "exit /b 7")); err == nil {
		t.Error("runner swallowed a non-zero exit status")
	}
}

// --- config_get / config_set ---

func TestConfigGetWithoutAKeyReturnsTheWholeConfig(t *testing.T) {
	cfg := testConfig(t)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	get := NewConfigTools(cfg)[0]

	res := get.Execute(context.Background(), map[string]any{})
	if res.IsError {
		t.Fatalf("res = %+v", res)
	}
	var full map[string]any
	if err := json.Unmarshal([]byte(res.ForLLM), &full); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, res.ForLLM)
	}
	for _, section := range []string{"agent", "provider", "tools", "heartbeat", "gateway"} {
		if _, ok := full[section]; !ok {
			t.Errorf("full config is missing the %q section", section)
		}
	}
}

func TestConfigGetRejectsAnUnreadableConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := (&configGetTool{path: path}).Execute(context.Background(), map[string]any{})
	if !res.IsError {
		t.Fatalf("res = %+v, want a parse error", res)
	}
	if !strings.Contains(res.ForLLM, "parse") {
		t.Errorf("error = %q, want it to name the parse failure", res.ForLLM)
	}
}

func TestConfigGetReadsTheFileNotTheLiveConfig(t *testing.T) {
	cfg := testConfig(t)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	get := NewConfigTools(cfg)[0]
	set := NewConfigTools(cfg)[1]

	if res := set.Execute(context.Background(), map[string]any{
		"key": "heartbeat.interval_minutes", "value": 45.0,
	}); res.IsError {
		t.Fatalf("set = %+v", res)
	}
	res := get.Execute(context.Background(), map[string]any{"key": "heartbeat.interval_minutes"})
	if res.IsError || strings.TrimSpace(res.ForLLM) != "45" {
		t.Errorf("res = %+v, want config_get to observe the persisted value", res)
	}
}

func TestConfigSetRejectsAnEmptyKey(t *testing.T) {
	set := NewConfigTools(testConfig(t))[1]
	res := set.Execute(context.Background(), map[string]any{"value": 1.0})
	if !res.IsError || !strings.Contains(res.ForLLM, "key is required") {
		t.Errorf("res = %+v", res)
	}
}

func TestConfigSetOnAnUnwritablePathFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := (&configSetTool{path: path}).Execute(context.Background(), map[string]any{
		"key": "heartbeat.interval_minutes", "value": 5.0,
	})
	if !res.IsError {
		t.Errorf("res = %+v, want the corrupt file to surface as an error", res)
	}
}

// --- DetectSystemManager ---

// The wizard names the exact packages a distribution needs, so the manager it
// detects has to be the one this machine really has. The probe order matters
// on boxes carrying more than one: the distribution's own manager wins.
func TestDetectSystemManagerPicksTheDistributionsOwn(t *testing.T) {
	fakeManagersOnPath(t, "pkg", "apt-get") // Puppy's pkg beside a stray apt-get
	if got := DetectSystemManager(); got != "apt" {
		t.Errorf("manager = %q, want apt — it is probed before pkg", got)
	}

	fakeManagersOnPath(t, "pkg")
	if got := DetectSystemManager(); got != "pkg" {
		t.Errorf("manager = %q, want pkg", got)
	}
}

// A machine with no supported system manager must say so rather than name one
// whose install command would fail — that answer is what makes the wizard
// offer a different route instead of a command that cannot work.
func TestDetectSystemManagerReportsNothingWhenNoneIsInstalled(t *testing.T) {
	fakeManagersOnPath(t)
	if got := DetectSystemManager(); got != "" {
		t.Errorf("manager = %q on a machine with none installed", got)
	}
}

// Only system managers are detected here: pip and npm install for a user, and
// naming one as *the* system manager would send a distribution package to the
// wrong installer.
func TestDetectSystemManagerIgnoresUserLevelInstallers(t *testing.T) {
	fakeManagersOnPath(t, "pip", "pipx", "uv", "npm")
	if got := DetectSystemManager(); got != "" {
		t.Errorf("manager = %q, want none — those are user-level installers", got)
	}
}

// fakeManagersOnPath makes PATH contain exactly the named probe binaries, so
// detection is decided by the test rather than by the host machine.
func fakeManagersOnPath(t *testing.T, probes ...string) {
	t.Helper()
	dir := t.TempDir()
	// Windows resolves a bare name through PATHEXT, so an extensionless file
	// is invisible to LookPath there however executable its mode bits are.
	suffix, body := "", "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		suffix, body = ".bat", "@exit /b 0\r\n"
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	}
	for _, name := range probes {
		if err := os.WriteFile(filepath.Join(dir, name+suffix), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}
