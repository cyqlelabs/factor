package voice

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func vec(values ...float64) []float64 { return values }

func TestSpeakerStoreEnrollsAndRecognizes(t *testing.T) {
	store := newSpeakerStore(t.TempDir())

	first := store.enroll(vec(1, 0, 0))
	second := store.enroll(vec(0, 1, 0))
	if first != "speaker-1" || second != "speaker-2" {
		t.Errorf("enrolled as %q and %q", first, second)
	}
	if !store.isPrimary(first) || store.isPrimary(second) {
		t.Error("the first voice enrolled is the primary one")
	}

	m := store.match(vec(0.9, 0.1, 0))
	if m.name != "speaker-1" || m.best < 0.9 {
		t.Errorf("match = %q at %.2f", m.name, m.best)
	}
	// Two profiles, and this reading is plainly one of them: the runner-up
	// has to be far enough back for the caller to act on the winner.
	if m.best-m.runnerUp < speakerRunnerUpMargin {
		t.Errorf("runner-up %.2f sits too close behind %.2f to name anybody", m.runnerUp, m.best)
	}
}

func TestSpeakerStoreMatchOnAnEmptyStore(t *testing.T) {
	store := newSpeakerStore(t.TempDir())
	if m := store.match(vec(1, 0)); m.name != "" || m.best != 0 {
		t.Errorf("match on empty = %q, %.2f", m.name, m.best)
	}
}

func TestSpeakerStoreLearnsAVoiceOverTime(t *testing.T) {
	home := t.TempDir()
	store := newSpeakerStore(home)
	store.enroll(vec(1, 0, 0))

	// The voice drifts; the profile follows it.
	store.learn("speaker-1", vec(0, 1, 0))
	if got := store.match(vec(0, 1, 0)).best; got <= 0.5 {
		t.Errorf("after learning, similarity to the new voice = %.2f", got)
	}

	// Everything survives a restart.
	reloaded := newSpeakerStore(home)
	if name := reloaded.match(vec(0, 1, 0)).name; name != "speaker-1" {
		t.Errorf("reloaded match = %q", name)
	}
	if got := reloaded.list(); len(got) != 1 || got[0].Utterances != 2 {
		t.Errorf("reloaded profiles = %+v", got)
	}
}

func TestSpeakerStoreRenameAndForget(t *testing.T) {
	home := t.TempDir()
	store := newSpeakerStore(home)
	store.enroll(vec(1, 0))
	store.enroll(vec(0, 1))

	if err := store.rename("speaker-2", "Roxana"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := store.rename("nobody", "x"); err == nil {
		t.Error("renaming an unknown profile did not fail")
	}
	if err := store.rename("speaker-1", "!!!"); err == nil {
		t.Error("a name with no letters or digits was accepted")
	}
	if err := store.rename("speaker-1", "Roxana!"); err == nil {
		t.Error("a name that slugs identically to another speaker's was accepted")
	}
	if name := store.match(vec(0, 1)).name; name != "Roxana" {
		t.Errorf("after rename, match = %q", name)
	}

	if err := store.forget("Roxana"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if name := newSpeakerStore(home).match(vec(0, 1)).name; name == "Roxana" {
		t.Error("a forgotten profile still matches after reload")
	}
}

func TestSpeakerStoreEnrollNamesNeverCollide(t *testing.T) {
	store := newSpeakerStore(t.TempDir())
	store.enroll(vec(1, 0))
	store.enroll(vec(0, 1))
	if err := store.forget("speaker-1"); err != nil {
		t.Fatal(err)
	}
	if name := store.enroll(vec(1, 1)); name != "speaker-3" {
		t.Errorf("enroll after forget = %q, want a fresh number", name)
	}
}

func TestSpeakerStoreSurvivesACorruptFile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, speakersFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newSpeakerStore(home)
	if name := store.enroll(vec(1, 0)); name != "speaker-1" {
		t.Errorf("enroll on a corrupt store = %q", name)
	}
}

func TestCosine(t *testing.T) {
	if c := cosine(vec(1, 0), vec(1, 0)); math.Abs(c-1) > 1e-9 {
		t.Errorf("identical vectors = %.4f", c)
	}
	if c := cosine(vec(1, 0), vec(0, 1)); math.Abs(c) > 1e-9 {
		t.Errorf("orthogonal vectors = %.4f", c)
	}
	if c := cosine(vec(1, 0), nil); c != 0 {
		t.Errorf("cosine against nothing = %.4f", c)
	}
}

func TestSpeakerSlug(t *testing.T) {
	cases := map[string]string{
		"Roxana":    "roxana",
		"speaker-2": "speaker-2",
		"Ana María": "ana-maria",
	}
	for name, want := range cases {
		if got := speakerSlug(name); got != want {
			t.Errorf("speakerSlug(%q) = %q, want %q", name, got, want)
		}
	}
}

// Vectors from two different embedding models are not comparable — where the
// dimensions differ the cosine is a flat zero, and where they agree it is
// noise. A profile carried across a model change would never match its owner
// again while quietly competing with the new one they get enrolled into.
func TestSpeakerStoreClearsProfilesWhenTheModelChanges(t *testing.T) {
	home := t.TempDir()
	s := newSpeakerStore(home)
	s.useModel("model-a")
	s.enroll([]float64{1, 0, 0})
	s.enroll([]float64{0, 1, 0})
	if !s.hasProfiles() {
		t.Fatal("nothing enrolled")
	}

	s.useModel("model-a") // the same model must change nothing
	if len(s.list()) != 2 {
		t.Errorf("re-reporting the same model disturbed the store: %+v", s.list())
	}

	s.useModel("model-b")
	if s.hasProfiles() {
		t.Errorf("profiles from another model survived: %+v", s.list())
	}
	// The change is durable, and the enrolment counter is not reused: a name
	// the user still remembers must not come back attached to somebody else.
	if reloaded := newSpeakerStore(home); reloaded.hasProfiles() {
		t.Error("the cleared profiles came back from disk")
	} else if name := reloaded.enroll([]float64{0, 0, 1}); name != "speaker-3" {
		t.Errorf("the next voice enrolled as %q, reusing a name that was in use", name)
	}
}

// A store written before the model was recorded cannot be trusted against the
// current one: it is exactly the case where nobody knows what made it.
func TestSpeakerStoreClearsProfilesWithNoRecordedModel(t *testing.T) {
	s := newSpeakerStore(t.TempDir())
	s.enroll([]float64{1, 0, 0})
	s.useModel("model-a")
	if s.hasProfiles() {
		t.Error("profiles of unknown provenance were kept")
	}
}

// The branch a similarity alone cannot explain, asserted where it is decided:
// a reading equidistant from two profiles is refused rather than rounded to
// the nearer one, and it carries the runner-up that made it a refusal.
func TestNameVoiceRefusesAReadingBetweenTwoProfiles(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	cfg := validConfig()
	cfg.SpeakerThreshold = defaultSpeakerThreshold
	cfg.UnknownSpeaker = unknownEnroll
	v, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	v.speakers = newSpeakerStore(v.home)
	v.speakers.enroll(vec(1, 0, 0))
	v.speakers.enroll(vec(0, 1, 0))

	between := voiceReading{Seconds: 5, Embedding: vec(1, 1, 0)}
	who := v.nameVoice(context.Background(), between, true)
	if who.via != viaAmbiguous || who.name != "" {
		t.Errorf("a reading between two profiles was named: %+v", who)
	}
	if who.runnerUp == 0 || who.similarity != who.runnerUp {
		t.Errorf("the runner-up did not ride the decision: %+v", who)
	}
	if got := v.speakers.list(); len(got) != 2 {
		t.Errorf("an ambiguous reading minted a profile: %+v", got)
	}

	// Unmistakably the first profile: named, and the store follows it.
	clear := voiceReading{Seconds: 5, Embedding: vec(1, 0, 0)}
	if who := v.nameVoice(context.Background(), clear, true); who.name != "speaker-1" || who.via != viaMatch {
		t.Errorf("a clear reading was not named: %+v", who)
	}
}
