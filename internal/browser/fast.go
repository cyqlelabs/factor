//go:build !nobrowser

package browser

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

// The fast path exists because reading a page and driving a page cost wildly
// different amounts. Chromium keeps a renderer, a GPU process and a compositor
// alive to show pixels nobody looks at; Lightpanda runs the same JavaScript
// against a DOM and never renders, for a fraction of the memory. So when the
// agent only needs to know what a page says, it asks the cheap engine — and
// anything involving clicks, forms or screenshots still goes to the real
// browser, which is the only one that can do them.

// fastSession supervises the lightweight engine and the CDP connection to it.
// Everything is lazy: a session that never reads a page never starts one.
type fastSession struct {
	cfg config.BrowserConfig

	mu          sync.Mutex
	cmd         *exec.Cmd
	allocCancel context.CancelFunc
	tabCtx      context.Context
	tabCancel   context.CancelFunc
}

func newFastSession(cfg config.BrowserConfig) *fastSession { return &fastSession{cfg: cfg} }

// freePort asks the kernel for a port nobody is using. The engine needs an
// explicit one, and hard-coding 9222 would make the real browser attach to
// the fast engine instead of launching itself.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (f *fastSession) ensure() (context.Context, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tabCtx != nil && f.tabCtx.Err() == nil {
		return f.tabCtx, nil
	}
	f.teardownLocked()

	binary := f.cfg.FastCommand
	if binary == "" {
		binary = FastEngineBinary(config.Home())
	}
	if !executable(binary) {
		return nil, fmt.Errorf("the lightweight engine is not installed at %s — re-run `factor init`, or set browser.fast_path to false to stop offering this tool", binary)
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, "serve", "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	// Lightpanda phones home unless told not to. Factor picked a
	// de-Googled browser for the main path; the fast one gets the same
	// treatment.
	cmd.Env = append(os.Environ(), "LIGHTPANDA_DISABLE_TELEMETRY=true")
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the lightweight engine: %w", err)
	}
	f.cmd = cmd

	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForDevtools(endpoint, 15*time.Second); err != nil {
		f.teardownLocked()
		return nil, err
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), endpoint)
	f.allocCancel = allocCancel
	f.tabCtx, f.tabCancel = chromedp.NewContext(allocCtx)
	if err := chromedp.Run(f.tabCtx); err != nil {
		f.teardownLocked()
		return nil, fmt.Errorf("connecting to the lightweight engine: %w", err)
	}
	return f.tabCtx, nil
}

// waitForDevtools polls until the engine's CDP server answers. A cold start
// is milliseconds on a fast box and seconds on the slow ones Factor targets.
func waitForDevtools(endpoint string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if devtoolsAlive(endpoint) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the lightweight engine did not open its CDP port within %s", limit)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// teardownLocked stops the engine and releases the connection. The caller
// holds f.mu.
func (f *fastSession) teardownLocked() {
	if f.tabCancel != nil {
		f.tabCancel()
		f.tabCancel = nil
	}
	if f.allocCancel != nil {
		f.allocCancel()
		f.allocCancel = nil
	}
	f.tabCtx = nil
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
		_ = f.cmd.Wait()
	}
	f.cmd = nil
}

// Close stops the engine.
func (f *fastSession) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardownLocked()
}

// fastReadScript returns what a reader wants and nothing a renderer would
// add: the title, the visible text, and where the page can go next.
const fastReadScript = `(() => {
  const links = [];
  for (const a of document.querySelectorAll('a[href]')) {
    const text = (a.innerText || '').trim().replace(/\s+/g, ' ');
    if (text) links.push({ text: text.slice(0, 120), href: a.href });
    if (links.length >= 100) break;
  }
  const body = document.body ? (document.body.innerText || '') : '';
  return {
    title: document.title || '',
    url: location.href,
    text: body.replace(/\n{3,}/g, '\n\n').trim().slice(0, 20000),
    links,
  };
})()`

type fastPageRead struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
	Links []struct {
		Text string `json:"text"`
		Href string `json:"href"`
	} `json:"links"`
}

type fetchTool struct{ f *fastSession }

func (t *fetchTool) Name() string { return "browser_fetch" }
func (t *fetchTool) Description() string {
	return "Read one page with a lightweight engine that runs JavaScript but never renders: returns the title, text and links for a fraction of the memory a real browser costs. Use it to read or research a page. It cannot click, fill, or screenshot, and it keeps no session — use browser_navigate for anything interactive, and also whenever what comes back here is thin, blocked, or missing what you asked for."
}
func (t *fetchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "The page to read"},
		},
		"required": []any{"url"},
	}
}

func (t *fetchTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	url := strings.TrimSpace(tools.StringArg(args, "url"))
	if url == "" {
		return tools.Errorf("url is required")
	}
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}

	tabCtx, err := t.f.ensure()
	if err != nil {
		return tools.Errorf("%v", err)
	}
	runCtx, cancel := context.WithTimeout(tabCtx, 45*time.Second)
	defer cancel()

	var read fastPageRead
	done := make(chan error, 1)
	go func() {
		done <- chromedp.Run(runCtx,
			chromedp.Navigate(url),
			chromedp.Evaluate(fastReadScript, &read),
		)
	}()
	select {
	case err := <-done:
		if err != nil {
			return tools.Errorf("reading %s: %v", url, err)
		}
	case <-ctx.Done():
		cancel()
		return tools.Errorf("reading %s: %v", url, ctx.Err())
	}

	return tools.Textf("%s", formatFastRead(&read))
}

func formatFastRead(r *fastPageRead) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\n%s\n", r.Title, r.URL, r.Text)
	if len(r.Links) > 0 {
		b.WriteString("\nLinks:\n")
		for _, l := range r.Links {
			fmt.Fprintf(&b, "  %s — %s\n", l.Text, l.Href)
		}
	}
	return b.String()
}
