package memory

import (
	"testing"

	"github.com/cyqlelabs/factor/internal/tools"
)

func TestGuestSpace(t *testing.T) {
	p, err := NewSpacePolicy("origin", "main", "system", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if sc, ok := p.Scope("a2a", tools.AudienceGuest); ok || sc.Space != "" {
		t.Errorf("a guest recalled with no guest space: %+v", sc)
	}
	g, err := p.WithGuest("guest")
	if err != nil {
		t.Fatal(err)
	}
	sc, ok := g.Scope("a2a", tools.AudienceGuest)
	if !ok || sc.Space != "guest" || len(sc.ReadSpaces) != 1 || sc.ReadSpaces[0] != "guest" {
		t.Errorf("guest scope %+v", sc)
	}
	for _, clash := range []string{"main", "system", "shared"} {
		if _, err := p.WithGuest(clash); err == nil {
			t.Errorf("guest space %q shared with another", clash)
		}
	}
	single, _ := NewSpacePolicy("single", "main", "system", "")
	if _, ok := single.Scope("a2a", tools.AudienceGuest); ok {
		t.Error("a single-space engine served a guest")
	}
	tc := tools.ToolContext{Audience: tools.AudienceGuest}
	if refuseCrossAudience(g, tc, "main") == nil || refuseCrossAudience(g, tc, "guest") != nil {
		t.Error("a guest's explicit space")
	}
}
