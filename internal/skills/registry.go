package skills

// The public skill registry. skills.sh is the directory the official `skills`
// CLI searches — ~34k skills from ~2.8k repositories, the widest and freshest
// index there is — and this speaks the two endpoints that CLI itself calls.
// Its documented /api/v1 is not one of them: that one answers 401 without a
// Vercel OIDC token tied to a deployed project, which a personal agent on
// someone's laptop cannot produce. BaseURL is configuration
// (tools.skill_registry_url) so a mirror, a private index, or whatever the
// registry renames these to can take over without a new tool.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRegistryURL is the public registry both skill tools talk to.
	DefaultRegistryURL = "https://skills.sh"

	registryTimeout = 30 * time.Second
	registryMaxBody = 8 << 20 // a skill is prose and small scripts
	registryAgent   = "factor-agent/1.0"
)

// Registry is the HTTP client the find and install tools share.
type Registry struct {
	BaseURL string       // empty means DefaultRegistryURL
	Client  *http.Client // nil means a client with registryTimeout
}

func NewRegistry(baseURL string) *Registry { return &Registry{BaseURL: baseURL} }

func (r *Registry) base() string {
	if r != nil && strings.TrimSpace(r.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(r.BaseURL), "/")
	}
	return DefaultRegistryURL
}

func (r *Registry) client() *http.Client {
	if r != nil && r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: registryTimeout}
}

// RegistrySkill is one search hit. The registry sends more fields than these
// and may send fewer: everything past ID is decoration the rendering skips
// when it is missing, so a payload change cannot break the search.
type RegistrySkill struct {
	ID          string `json:"id"`          // owner/repo/skill — the slug skill_install takes
	Name        string `json:"name"`        //
	Description string `json:"description"` // not in today's payload; printed when it appears
	Source      string `json:"source"`      // owner/repo
	Installs    int    `json:"installs"`
}

// RegistryFile is one file of a skill's snapshot, its path relative to the
// skill directory ("SKILL.md", "scripts/run.py").
type RegistryFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

// Search asks the registry for skills matching query, optionally narrowed to
// one GitHub owner. The order is the registry's own ranking.
func (r *Registry) Search(ctx context.Context, query, owner string, limit int) ([]RegistrySkill, error) {
	params := url.Values{"q": {query}, "limit": {strconv.Itoa(limit)}}
	if owner != "" {
		params.Set("owner", owner)
	}
	var payload struct {
		Skills []RegistrySkill `json:"skills"`
	}
	if err := r.getJSON(ctx, r.base()+"/api/search?"+params.Encode(), &payload); err != nil {
		return nil, err
	}
	return payload.Skills, nil
}

// Download returns every file of the skill named by an owner/repo/skill slug.
// The snapshot is ref-agnostic and needs no git, no GitHub token and no clone
// of a repository that may hold two hundred other skills.
func (r *Registry) Download(ctx context.Context, slug string) ([]RegistryFile, error) {
	parts := strings.Split(slug, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%q is not an owner/repo/skill slug", slug)
	}
	endpoint := r.base() + "/api/download/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/" + url.PathEscape(parts[2])
	var payload struct {
		Files []RegistryFile `json:"files"`
	}
	if err := r.getJSON(ctx, endpoint, &payload); err != nil {
		return nil, err
	}
	if len(payload.Files) == 0 {
		return nil, fmt.Errorf("registry has no files for %q", slug)
	}
	return payload.Files, nil
}

// getJSON reports the registry's own failures as themselves: a personal agent
// that says "search failed" when the answer is "this host is not reachable
// from here" sends its user looking in the wrong place.
func (r *Registry) getJSON(ctx context.Context, endpoint string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", registryAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := r.client().Do(req)
	if err != nil {
		return fmt.Errorf("%s is not reachable: %w", r.base(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, registryMaxBody))
	if err != nil {
		return fmt.Errorf("reading %s: %w", r.base(), err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s answered HTTP %d", r.base(), resp.StatusCode)
	}
	// A body at the cap was cut mid-JSON: say that, rather than blaming the
	// registry for malformed output we are the ones who truncated.
	if len(body) >= registryMaxBody {
		return fmt.Errorf("%s answered more than %d bytes; too large for one skill", r.base(), registryMaxBody)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s answered something that is not the JSON expected: %w", r.base(), err)
	}
	return nil
}
