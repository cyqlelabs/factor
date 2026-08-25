//go:build !nobrowser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// listingPage is the shape that broke: a site whose navigation, skip links
// and filters all come before its content in the DOM. Reading the first N
// elements in document order returns the furniture and none of the results,
// which is exactly what a real listing page did.
func listingPage() string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><title>Shop</title></head><body>`)
	b.WriteString(`<header><nav>`)
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&b, `<a href="/nav/%d">Nav link %d</a>`, i, i)
	}
	b.WriteString(`</nav></header><main><aside>`)
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, `<a href="/filter/%d">Filter %d</a>`, i, i)
	}
	b.WriteString(`</aside><div id="results">`)
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&b, `<a href="/p/%d">Widget %d costs $%d0</a>`, i, i, i)
	}
	b.WriteString(`</div><div style="height:4000px">tall</div></main>`)
	b.WriteString(`<footer><a href="/about">About us</a></footer>`)
	// Content that only exists once something scrolls, the way an endless
	// listing behaves.
	b.WriteString(`<script>
	  addEventListener('scroll', () => {
	    if (window.scrollY > 50 && !window.grown) {
	      window.grown = true;
	      const d = document.createElement('div');
	      d.style.height = '3000px';
	      d.innerHTML = '<a href="/p/lazy">Lazy widget costs $999</a>';
	      document.getElementById('results').appendChild(d);
	    }
	  });
	</script></body></html>`)
	return b.String()
}

func serveListing(t *testing.T) *httptest.Server {
	t.Helper()
	page := listingPage()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// elements returns just the ref listing, which is what these tests are about:
// the page text above it mentions the same words and would answer for them.
func elements(read string) string {
	_, listing, ok := strings.Cut(read, "Interactive elements (")
	if !ok {
		return ""
	}
	return listing
}

// firstIndexOf reports where a substring lands in the element listing, so a
// test can assert on order rather than on mere presence.
func firstIndexOf(read, want string) int {
	for i, line := range strings.Split(elements(read), "\n") {
		if strings.Contains(line, want) {
			return i
		}
	}
	return -1
}

func TestReadPutsTheContentAheadOfTheFurniture(t *testing.T) {
	requireBrowser(t)
	srv := serveListing(t)
	byName := liveSuite(t)
	ctx := context.Background()

	res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL})
	if res.IsError {
		if strings.Contains(res.ForLLM, "browser start") {
			t.Skipf("chrome cannot start here: %s", res.ForLLM)
		}
		t.Fatalf("navigate: %s", res.ForLLM)
	}

	// 150 navigation links precede the results in the DOM, so document order
	// alone cannot reach a product inside any sane budget.
	product := firstIndexOf(res.ForLLM, "Widget 1 costs")
	if product < 0 {
		t.Fatalf("no product reached the model:\n%s", res.ForLLM)
	}
	if nav := firstIndexOf(res.ForLLM, "Nav link 1"); nav >= 0 && nav < product {
		t.Errorf("site navigation (line %d) came before the results (line %d)", nav, product)
	}
	// The filters live in the same main region, and belong after the content.
	if filter := firstIndexOf(res.ForLLM, "Filter 1"); filter >= 0 && filter < product {
		t.Errorf("filters (line %d) came before the results (line %d)", filter, product)
	}
	// What it left out is stated, so an empty-looking page can be told apart
	// from a page whose rest was not shown.
	if !strings.Contains(res.ForLLM, "on the page") {
		t.Errorf("the read did not say how much it held back:\n%s", firstLines(res.ForLLM, 6))
	}
}

func TestReadNarrowsToWhatWasAskedFor(t *testing.T) {
	requireBrowser(t)
	srv := serveListing(t)
	byName := liveSuite(t)
	ctx := context.Background()
	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL}); res.IsError {
		if strings.Contains(res.ForLLM, "browser start") {
			t.Skipf("chrome cannot start here: %s", res.ForLLM)
		}
		t.Fatalf("navigate: %s", res.ForLLM)
	}

	res := byName["browser_read"].Execute(ctx, map[string]any{"filter": "widget"})
	if res.IsError {
		t.Fatalf("filtered read: %s", res.ForLLM)
	}
	if listing := elements(res.ForLLM); strings.Contains(listing, "Nav link") || strings.Contains(listing, "About us") {
		t.Errorf("the filter let unrelated elements through:\n%s", res.ForLLM)
	}
	if got := strings.Count(elements(res.ForLLM), "Widget "); got != 12 {
		t.Errorf("filtered read listed %d widgets, want 12:\n%s", got, res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "filtered") {
		t.Errorf("a filtered read did not say it was filtered:\n%s", firstLines(res.ForLLM, 6))
	}

	// A limit below what matches is reported rather than silently applied.
	res = byName["browser_read"].Execute(ctx, map[string]any{"filter": "widget", "limit": 3})
	if got := strings.Count(elements(res.ForLLM), "Widget "); got != 3 {
		t.Errorf("limited read listed %d widgets, want 3", got)
	}
	if !strings.Contains(res.ForLLM, "3 shown of 12 matching") {
		t.Errorf("the limit was applied silently:\n%s", firstLines(res.ForLLM, 6))
	}
}

func TestScrollReachesWhatOnlyLoadsOnTheWayDown(t *testing.T) {
	requireBrowser(t)
	srv := serveListing(t)
	byName := liveSuite(t)
	ctx := context.Background()
	if res := byName["browser_navigate"].Execute(ctx, map[string]any{"url": srv.URL}); res.IsError {
		if strings.Contains(res.ForLLM, "browser start") {
			t.Skipf("chrome cannot start here: %s", res.ForLLM)
		}
		t.Fatalf("navigate: %s", res.ForLLM)
	}
	if res := byName["browser_read"].Execute(ctx, map[string]any{"filter": "lazy"}); strings.Contains(elements(res.ForLLM), "Lazy widget") {
		t.Fatal("the lazy content was present before any scroll; the test page is not testing anything")
	}

	res := byName["browser_scroll"].Execute(ctx, map[string]any{"to": "bottom", "filter": "widget"})
	if res.IsError {
		t.Fatalf("scroll: %s", res.ForLLM)
	}
	if !strings.Contains(elements(res.ForLLM), "Lazy widget") {
		t.Errorf("scrolling did not reach the content that loads on the way down:\n%s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "loaded more of the page") {
		t.Errorf("a scroll that grew the page did not say so:\n%s", firstLines(res.ForLLM, 4))
	}

	// Going back up is the same tool and lands somewhere readable.
	if res := byName["browser_scroll"].Execute(ctx, map[string]any{"to": "top"}); res.IsError {
		t.Errorf("scroll to top: %s", res.ForLLM)
	}
	if res := byName["browser_scroll"].Execute(ctx, map[string]any{"to": "up"}); res.IsError {
		t.Errorf("scroll up: %s", res.ForLLM)
	}
	if res := byName["browser_scroll"].Execute(ctx, nil); res.IsError {
		t.Errorf("default scroll: %s", res.ForLLM)
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// hydratingPage is the other shape that broke: a listing whose server answer
// is an empty <main> shell, filled in by script after the load event. The
// read that follows a navigation used to fire the moment the shell arrived,
// see an empty main region beside real site furniture, and hand the model a
// page with "no results" that the user was looking at.
const hydratingPage = `<!doctype html><html><head><title>Late Shop</title></head><body>
<nav><a href="/home">Home</a><a href="/help">Help</a></nav>
<main id="m"></main>
<script>
  setTimeout(() => {
    document.getElementById('m').innerHTML =
      '<p>1.077 resultados</p><a href="/p/MLA1">Malbec Pack x6 costs $100</a>';
  }, 1200);
</script></body></html>`

func TestNavigateWaitsForAPageThatRendersLate(t *testing.T) {
	requireBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, hydratingPage)
	}))
	t.Cleanup(srv.Close)
	byName := liveSuite(t)

	res := byName["browser_navigate"].Execute(context.Background(), map[string]any{"url": srv.URL})
	if res.IsError {
		if strings.Contains(res.ForLLM, "browser start") {
			t.Skipf("chrome cannot start here: %s", res.ForLLM)
		}
		t.Fatalf("navigate: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Malbec Pack x6") {
		t.Fatalf("the late-rendered results never reached the model:\n%s", res.ForLLM)
	}
	// A content link says where it goes, so opening a result never needs a
	// guessed URL.
	if !strings.Contains(res.ForLLM, `"Malbec Pack x6 costs $100" → /p/MLA1`) {
		t.Errorf("the result link hid its target:\n%s", res.ForLLM)
	}
}
