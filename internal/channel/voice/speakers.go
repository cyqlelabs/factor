package voice

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Speaker profiles: who is talking. Each profile is one voice — a unit-length
// embedding centroid the speech server computed — persisted under Factor's
// home so the household survives a restart. Matching is cosine similarity
// against every profile; what similarity is enough is the caller's threshold
// to hold, not the store's.

// speakersFile sits beside config.json, like usage.json and last-channel.json.
const speakersFile = "voice-speakers.json"

// speakerLearnCap bounds how much one utterance can move a settled profile:
// the running mean becomes an EMA once this many utterances are in.
const speakerLearnCap = 20

type speakerProfile struct {
	Name string `json:"name"`
	// Primary marks the machine's owner — the first voice enrolled. The
	// primary speaker keeps the channel's original session rather than
	// getting one of their own.
	Primary    bool      `json:"primary,omitempty"`
	Embedding  []float64 `json:"embedding"`
	Utterances int       `json:"utterances"`
}

type speakerStore struct {
	mu   sync.Mutex
	path string
	// enrolled counts every profile ever created, surviving forgets, so a
	// forgotten speaker's number is never reissued to somebody else.
	enrolled int
	profiles []*speakerProfile
}

// speakersOnDisk is the file's shape.
type speakersOnDisk struct {
	Enrolled int               `json:"enrolled"`
	Profiles []*speakerProfile `json:"profiles"`
}

// newSpeakerStore loads the profiles under home, starting empty when there are
// none — or when the file is beyond reading, which must not take voice down.
func newSpeakerStore(home string) *speakerStore {
	s := &speakerStore{path: filepath.Join(home, speakersFile)}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	var disk speakersOnDisk
	if err := json.Unmarshal(raw, &disk); err != nil {
		return s
	}
	s.enrolled = disk.Enrolled
	s.profiles = disk.Profiles
	return s
}

// match returns the closest profile and how close it is; "" when none exist.
func (s *speakerStore) match(embedding []float64) (string, float64) {
	embedding = unit(embedding)
	s.mu.Lock()
	defer s.mu.Unlock()
	best, bestSim := "", 0.0
	for _, p := range s.profiles {
		if sim := cosine(embedding, p.Embedding); best == "" || sim > bestSim {
			best, bestSim = p.Name, sim
		}
	}
	return best, bestSim
}

// scores is every profile's similarity to one embedding, formatted for a
// debug line — "roxana=0.81 speaker-1=0.42", ordered best first. It exists
// for tuning speaker_threshold: the decision reports its winner, and this
// reports how far behind the runners-up were.
func (s *speakerStore) scores(embedding []float64) string {
	embedding = unit(embedding)
	s.mu.Lock()
	type score struct {
		name string
		sim  float64
	}
	scored := make([]score, 0, len(s.profiles))
	for _, p := range s.profiles {
		scored = append(scored, score{p.Name, cosine(embedding, p.Embedding)})
	}
	s.mu.Unlock()
	if len(scored) == 0 {
		return "no profiles enrolled"
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].sim > scored[j].sim })
	var b strings.Builder
	for i, sc := range scored {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%.2f", sc.name, sc.sim)
	}
	return b.String()
}

// learn folds one more utterance of a known voice into its profile.
func (s *speakerStore) learn(name string, embedding []float64) {
	embedding = unit(embedding)
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(name)
	if p == nil {
		return
	}
	alpha := 1.0 / float64(min(p.Utterances+1, speakerLearnCap))
	for i := range p.Embedding {
		if i < len(embedding) {
			p.Embedding[i] += alpha * (embedding[i] - p.Embedding[i])
		}
	}
	p.Embedding = unit(p.Embedding)
	p.Utterances++
	s.saveLocked()
}

// enroll creates a profile for a voice heard for the first time. The first
// voice the machine ever hears is its owner's: that profile is primary.
func (s *speakerStore) enroll(embedding []float64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrolled++
	p := &speakerProfile{
		Name:       fmt.Sprintf("speaker-%d", s.enrolled),
		Primary:    len(s.profiles) == 0,
		Embedding:  unit(embedding),
		Utterances: 1,
	}
	s.profiles = append(s.profiles, p)
	s.saveLocked()
	return p.Name
}

func (s *speakerStore) rename(from, to string) error {
	to = strings.TrimSpace(to)
	if to == "" || speakerSlug(to) == "" {
		return fmt.Errorf("a speaker needs a name with letters or digits in it")
	}
	// The shared room holds a session under this slug, so a speaker renamed
	// into it would inherit the whole room's conversation.
	if speakerSlug(to) == roomSessionSlug {
		return fmt.Errorf("%q is reserved for the shared-room conversation", to)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Names key sessions by their slug, so two names must not collapse into
	// one: "Ana!" beside "Ana" would silently share a conversation.
	for _, p := range s.profiles {
		if p.Name != from && speakerSlug(p.Name) == speakerSlug(to) {
			return fmt.Errorf("%q would share a session with the speaker named %q", to, p.Name)
		}
	}
	p := s.find(from)
	if p == nil {
		return fmt.Errorf("no speaker named %q", from)
	}
	p.Name = to
	s.saveLocked()
	return nil
}

func (s *speakerStore) forget(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.profiles {
		if p.Name == name {
			s.profiles = append(s.profiles[:i], s.profiles[i+1:]...)
			s.saveLocked()
			return nil
		}
	}
	return fmt.Errorf("no speaker named %q", name)
}

// hasProfiles reports whether any voice is enrolled. The room reads it to
// decide what an unmatched voice means: with a profile on file the owner
// would have matched, so an unmatched voice is somebody else; with none, it
// is simply the first voice this machine ever heard.
func (s *speakerStore) hasProfiles() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.profiles) > 0
}

func (s *speakerStore) isPrimary(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(name)
	return p != nil && p.Primary
}

// speakerInfo is a profile without its embedding — what lists and tools see.
type speakerInfo struct {
	Name       string `json:"name"`
	Primary    bool   `json:"primary,omitempty"`
	Utterances int    `json:"utterances"`
}

func (s *speakerStore) list() []speakerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	infos := make([]speakerInfo, 0, len(s.profiles))
	for _, p := range s.profiles {
		infos = append(infos, speakerInfo{Name: p.Name, Primary: p.Primary, Utterances: p.Utterances})
	}
	return infos
}

// summary names the enrolled voices in one log-line string, the primary one
// marked: "nicolas*, roxana (12 utterances each)" is unreadable, so it stays
// to names — "none" when nobody is enrolled yet.
func (s *speakerStore) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.profiles) == 0 {
		return "none"
	}
	names := make([]string, 0, len(s.profiles))
	for _, p := range s.profiles {
		name := fmt.Sprintf("%s(%d)", p.Name, p.Utterances)
		if p.Primary {
			name += "*"
		}
		names = append(names, name)
	}
	return strings.Join(names, " ")
}

func (s *speakerStore) find(name string) *speakerProfile {
	for _, p := range s.profiles {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// saveLocked writes the store out; a write that fails costs recognition
// history, not the conversation.
func (s *speakerStore) saveLocked() {
	raw, err := json.MarshalIndent(speakersOnDisk{Enrolled: s.enrolled, Profiles: s.profiles}, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// cosine is the similarity of two vectors; the store keeps its centroids at
// unit length, so against them it is a plain dot product with a guard.
func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

// unit scales a vector to length one, in a copy.
func unit(v []float64) []float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	out := make([]float64, len(v))
	if sum == 0 {
		return out
	}
	norm := math.Sqrt(sum)
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// speakerSlug turns a profile name into the session-key suffix: lowercase,
// accents folded, anything else a dash — so "Ana María" keys as "ana-maria".
func speakerSlug(name string) string {
	folded := strings.NewReplacer(
		"á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n",
		"à", "a", "è", "e", "ì", "i", "ò", "o", "ù", "u", "ç", "c",
	).Replace(strings.ToLower(name))
	var b strings.Builder
	lastDash := true
	for _, r := range folded {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
