//go:build !nobrowser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveCacheablePages is the realistic case: no cache-busting headers, so
// Chrome puts the first page in the back/forward cache. A bfcache restore
// never fires the lifecycle event chromedp.NavigateBack waits for, which is
// what used to make browser_back burn its full timeout.
func serveCacheablePages(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/second" {
			fmt.Fprint(w, `<html><head><title>Second Page</title></head><body><p>second</p></body></html>`)
			return
		}
		fmt.Fprint(w, `<html><head><title>First Page</title></head><body><p>first</p></body></html>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBrowserBackReturnsPromptlyFromTheBackForwardCache(t *testing.T) {
	requireBrowser(t)
	srv := serveCacheablePages(t)
	byName := liveSuite(t)
	ctx := context.Background()

	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL}); res.IsError {
		t.Fatalf("first navigate: %s", res.ForLLM)
	}
	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL + "/second"}); res.IsError {
		t.Fatalf("second navigate: %s", res.ForLLM)
	}

	start := time.Now()
	res := byName["browser_back"].Execute(ctx, nil)
	elapsed := time.Since(start)

	if res.IsError {
		t.Fatalf("back on a cacheable page failed: %s", res.ForLLM)
	}
	if elapsed > 10*time.Second {
		t.Errorf("back took %s — it stalled waiting for a lifecycle event the bfcache never fires", elapsed)
	}
	if !strings.Contains(res.ForLLM, "First Page") {
		t.Errorf("back did not land on the first page:\n%s", res.ForLLM)
	}
}
