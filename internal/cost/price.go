// Package cost prices what the agent spends. Every provider reports the
// tokens a call used; this package turns those counts into dollars, keeps a
// running ledger per session and overall, and refuses a turn once a budget
// cap is reached.
//
// Prices come from the model catalog OpenRouter publishes — the one public
// list that carries per-token rates for every major vendor — cached on disk
// so a cold start is not a network round trip. Models served from this
// machine are free by construction, and anything the catalog does not carry
// is counted in tokens and left unpriced rather than guessed at.
package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/provider"
)

// Price is what one model charges, in USD per million tokens.
type Price = config.Price

// USD converts a token count into money.
func usd(p Price, in, out int) float64 {
	return (float64(in)*p.Input + float64(out)*p.Output) / 1e6
}

// What a prompt cache does to the input rate. A hit is billed at a fraction of
// the normal rate and the write that creates one at a premium, so a request
// that repeats a cached prefix twice has already paid for the write. Neither
// number is Factor's to choose — they are what the endpoints charge — and a
// catalog that priced cached tokens at the full rate would report a turn as
// costing several times what it did.
const (
	cacheReadRate  = 0.10
	cacheWriteRate = 1.25
)

// usdUsage prices one call, discounting the input served from cache and
// surcharging the input written to it. The two cache counts are subsets of
// PromptTokens, so what is left after removing both is the part billed at the
// plain rate; a provider reporting neither prices exactly as it always did.
func usdUsage(p Price, u provider.Usage) float64 {
	cached, written := u.CacheReadTokens, u.CacheWriteTokens
	if cached < 0 {
		cached = 0
	}
	if written < 0 {
		written = 0
	}
	// A provider whose counters disagree with its own total is not worth
	// trusting into a negative bill: fall back to pricing everything plainly.
	if cached+written > u.PromptTokens {
		return usd(p, u.PromptTokens, u.CompletionTokens)
	}
	in := float64(u.PromptTokens-cached-written) +
		float64(cached)*cacheReadRate +
		float64(written)*cacheWriteRate
	return (in*p.Input + float64(u.CompletionTokens)*p.Output) / 1e6
}

// DefaultPricesURL is the public model catalog: ids with per-token rates for
// every vendor OpenRouter fronts, which is most of them.
const DefaultPricesURL = "https://openrouter.ai/api/v1/models"

// localTypes are the provider types that only ever serve models this machine
// (or its LAN) is already paying for in electricity.
var localTypes = map[string]bool{"ollama": true, "lmstudio": true, "llamacpp": true}

// Local reports whether a candidate is served locally, and therefore costs
// nothing per token. A type Factor knows to be local says so on its own; any
// other type is judged by where its endpoint points.
func Local(c config.Candidate) bool {
	if localTypes[c.Type] {
		return true
	}
	base := c.APIBase
	if base == "" {
		base = provider.DefaultAPIBase(c.Type)
	}
	return localEndpoint(base)
}

func localEndpoint(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// Model is one catalog row: what the model charges, and how much context it
// takes — the window compaction budgets against.
type Model struct {
	Price  Price `json:"price"`
	Window int   `json:"window,omitempty"`
}

// Catalog answers what a model costs and how much context it carries.
// Overrides from config win, models served locally are free, and everything
// else comes from the fetched catalog — by exact id first, then by the
// vendor-and-date-insensitive form that lets a native Anthropic model id find
// its OpenRouter twin.
type Catalog struct {
	path   string
	url    string
	ttl    time.Duration
	client *http.Client
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time

	override map[string]Price // config, keyed by exact lowercased id
	free     map[string]bool  // models a local candidate serves
	paid     bool             // at least one candidate bills per token

	mu      sync.RWMutex
	byID    map[string]Model
	byShort map[string]Model
	fetched time.Time
}

// cacheFile is the on-disk pricing cache.
type cacheFile struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Source    string           `json:"source"`
	Models    map[string]Model `json:"models"`
}

// NewCatalog builds the price book for one configuration. It reads whatever
// the last fetch cached, so pricing works before — and without — a network
// call.
func NewCatalog(cfg config.CostConfig, candidates []config.Candidate, cachePath string) *Catalog {
	c := &Catalog{
		path:     cachePath,
		url:      cfg.PricesURL,
		ttl:      time.Duration(cfg.RefreshHours) * time.Hour,
		client:   &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
		after:    time.After,
		override: map[string]Price{},
		free:     map[string]bool{},
		byID:     map[string]Model{},
		byShort:  map[string]Model{},
	}
	if c.url == "" {
		c.url = DefaultPricesURL
	}
	if c.ttl <= 0 {
		c.ttl = 24 * time.Hour
	}
	for id, p := range cfg.Prices {
		c.override[strings.ToLower(strings.TrimSpace(id))] = p
	}
	for _, cand := range candidates {
		if cand.Model == "" {
			continue
		}
		if Local(cand) {
			c.free[strings.ToLower(cand.Model)] = true
			continue
		}
		c.paid = true
	}
	c.loadCache()
	return c
}

// Paid reports whether any configured candidate bills per token — the answer
// to "is this machine spending money when it thinks?".
func (c *Catalog) Paid() bool { return c.paid }

// Price returns what a model charges. ok is false when nothing prices it,
// which the caller must report as unpriced rather than as free.
func (c *Catalog) Price(model string) (Price, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return Price{}, false
	}
	if p, ok := c.override[key]; ok {
		return p, true
	}
	if c.free[key] {
		return Price{}, true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if m, ok := c.byID[key]; ok {
		return m.Price, true
	}
	if m, ok := c.byShort[shortID(key)]; ok {
		return m.Price, true
	}
	return Price{}, false
}

// Window returns a model's context length in tokens, or 0 when the catalog
// does not know it — locally served models among them, whose window only
// their own configuration knows.
func (c *Catalog) Window(model string) int {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if m, ok := c.byID[key]; ok {
		return m.Window
	}
	if m, ok := c.byShort[shortID(key)]; ok {
		return m.Window
	}
	return 0
}

// dateSuffix is the release stamp vendors append to a model id
// (claude-sonnet-4-5-20250929, gpt-4o-2024-11-20).
var dateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2}|latest|v\d+)$`)

// shortID reduces a model id to the part two vendors are unlikely to share:
// no routing vendor prefix, no variant suffix, no release date.
func shortID(model string) string {
	s := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(s, ':'); i > 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return dateSuffix.ReplaceAllString(s, "")
}

// Fresh reports whether the cached catalog is young enough to skip a fetch.
func (c *Catalog) Fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byID) > 0 && c.now().Sub(c.fetched) < c.ttl
}

// Refresh fetches the catalog and caches it. It is a no-op when nothing here
// bills per token: a machine running only local models has no rates to look
// up and no reason to phone out.
func (c *Catalog) Refresh(ctx context.Context) error {
	if !c.paid || c.Fresh() {
		return nil
	}
	models, err := c.fetch(ctx)
	if err != nil {
		return err
	}
	c.adopt(models, c.now())
	return c.saveCache(models)
}

func (c *Catalog) fetch(ctx context.Context) (map[string]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching model prices: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching model prices: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("fetching model prices: %w", err)
	}
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("decoding model prices: %w", err)
	}
	models := make(map[string]Model, len(body.Data))
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		// Rates arrive as USD per token, in decimal strings small enough that
		// only a float parse reads them back.
		in, err1 := strconv.ParseFloat(m.Pricing.Prompt, 64)
		out, err2 := strconv.ParseFloat(m.Pricing.Completion, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		models[strings.ToLower(m.ID)] = Model{
			Price:  Price{Input: in * 1e6, Output: out * 1e6},
			Window: m.ContextLength,
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("model price catalog at %s carried no priced models", c.url)
	}
	return models, nil
}

// adopt installs a fetched catalog, building the short-id index alongside it.
// A short id two vendors both claim at different rates or windows is dropped:
// an unpriced model is a gap the usage report names, while a wrong price is a
// number nobody can tell is wrong.
func (c *Catalog) adopt(models map[string]Model, at time.Time) {
	short := make(map[string]Model, len(models))
	clashed := map[string]bool{}
	for id, m := range models {
		s := shortID(id)
		if s == "" || clashed[s] {
			continue
		}
		if prev, ok := short[s]; ok && prev != m {
			delete(short, s)
			clashed[s] = true
			continue
		}
		short[s] = m
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID, c.byShort, c.fetched = models, short, at
}

func (c *Catalog) loadCache() {
	if c.path == "" {
		return
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var f cacheFile
	// A cache in the pre-window format has no "models" key: it is simply
	// ignored, and the next refresh rewrites it.
	if err := json.Unmarshal(data, &f); err != nil || len(f.Models) == 0 {
		return
	}
	c.adopt(f.Models, f.FetchedAt)
}

func (c *Catalog) saveCache(models map[string]Model) error {
	if c.path == "" {
		return nil
	}
	data, err := json.Marshal(cacheFile{FetchedAt: c.now(), Source: c.url, Models: models})
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
