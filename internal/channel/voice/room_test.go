package voice

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// t0 is a fixed clock: every room assertion is about elapsed time, and a real
// one would make "the guest aged out" depend on how slow the test machine is.
var t0 = time.Date(2026, 8, 22, 20, 0, 0, 0, time.UTC)

func heard(name string, primary bool, via string) speakerIdentity {
	return speakerIdentity{name: name, primary: primary, via: via, similarity: 0.9}
}

// heardOne folds a single voice in — the shape most of these assertions are
// about, now that a recording can hold several.
func (r *room) heardOne(who speakerIdentity, hasProfiles bool, now time.Time) {
	r.heard([]speakerIdentity{who}, hasProfiles, now)
}

func TestRoomStartsPrivate(t *testing.T) {
	r := newRoom("", time.Hour)
	if st := r.snapshot(t0); st.Shared || len(st.Present) != 0 {
		t.Errorf("a fresh room = %+v, want private and empty", st)
	}
}

// The owner talking to themselves must never look like company: theirs is the
// baseline the room is measured against.
func TestOwnerIsNeverAnOccupant(t *testing.T) {
	r := newRoom("", time.Hour)
	r.heardOne(heard("nicolas", true, viaMatch), true, t0)
	if st := r.snapshot(t0); st.Shared {
		t.Errorf("the owner filled the room: %+v", st)
	}
}

func TestRecognizedGuestSharesTheRoom(t *testing.T) {
	r := newRoom("", time.Hour)
	r.heardOne(heard("roxana", false, viaMatch), true, t0)
	st := r.snapshot(t0)
	if !st.Shared || !reflect.DeepEqual(st.Present, []string{"roxana"}) {
		t.Errorf("room = %+v, want shared with roxana", st)
	}
}

// An unmatched voice is company only where the owner's voice would have
// matched. With nobody enrolled it is the machine's first voice, not its
// second, and calling that company would leave a one-person household
// permanently discreet with itself.
func TestUnknownVoiceNeedsAProfileToCount(t *testing.T) {
	withProfiles := newRoom("", time.Hour)
	withProfiles.heardOne(speakerIdentity{via: viaUnknown}, true, t0)
	if st := withProfiles.snapshot(t0); !st.Shared ||
		!reflect.DeepEqual(st.Present, []string{roomUnknownOccupant}) {
		t.Errorf("unmatched voice with profiles enrolled = %+v, want shared", st)
	}

	bare := newRoom("", time.Hour)
	bare.heardOne(speakerIdentity{via: viaUnknown}, false, t0)
	if st := bare.snapshot(t0); st.Shared {
		t.Errorf("unmatched voice with nobody enrolled = %+v, want private", st)
	}
}

// An embedding outage means the microphone went blind, not that the room
// filled up: it must not invent an occupant nobody heard.
func TestUnreadableAudioAddsNobody(t *testing.T) {
	for _, via := range []string{viaUnavailable, viaOverlap, viaShort} {
		r := newRoom("", time.Hour)
		r.heardOne(speakerIdentity{via: via}, true, t0)
		if st := r.snapshot(t0); st.Shared {
			t.Errorf("via %s filled the room: %+v", via, st)
		}
	}
}

// A guest whose every follow-up is "mhm" is never embedded, so an inherited
// name must keep them present — but it is not evidence anybody new arrived.
func TestInheritedNameRefreshesButNeverCreates(t *testing.T) {
	r := newRoom("", 10*time.Minute)
	r.heardOne(heard("roxana", false, viaMatch), true, t0)
	inherited := speakerIdentity{name: "roxana", via: viaShort, inherited: true}
	r.heardOne(inherited, true, t0.Add(9*time.Minute))
	if st := r.snapshot(t0.Add(15 * time.Minute)); !st.Shared {
		t.Errorf("a refreshed guest aged out anyway: %+v", st)
	}

	fresh := newRoom("", 10*time.Minute)
	fresh.heardOne(inherited, true, t0)
	if st := fresh.snapshot(t0); st.Shared {
		t.Errorf("an inherited name created an occupant: %+v", st)
	}
}

func TestOccupantsAgeOut(t *testing.T) {
	r := newRoom("", 30*time.Minute)
	r.heardOne(heard("roxana", false, viaEnrolled), true, t0)
	if st := r.snapshot(t0.Add(29 * time.Minute)); !st.Shared {
		t.Error("the guest left early")
	}
	if st := r.snapshot(t0.Add(31 * time.Minute)); st.Shared {
		t.Errorf("the guest never left: %+v", st)
	}
}

func TestDeclareOutranksTheMicrophone(t *testing.T) {
	r := newRoom("", time.Hour)

	// Somebody who walked in without speaking is invisible to sound.
	r.declare(true, []string{"roxana", "  "}, t0)
	if st := r.snapshot(t0); !reflect.DeepEqual(st.Present, []string{"roxana"}) {
		t.Errorf("declared company = %+v", st)
	}

	// Departure is announced by nothing at all, so saying so is the only way
	// to get the room back before the timeout.
	r.declare(false, nil, t0)
	if st := r.snapshot(t0); st.Shared {
		t.Errorf("declaring the room empty left %+v", st)
	}

	// Company with nobody named is still company.
	r.declare(true, nil, t0)
	if st := r.snapshot(t0); !reflect.DeepEqual(st.Present, []string{roomUnknownOccupant}) {
		t.Errorf("unnamed company = %+v", st)
	}
}

func TestForgetDropsOnePersonAndLeavesTheRest(t *testing.T) {
	r := newRoom("", time.Hour)
	r.declare(true, []string{"roxana", "ana"}, t0)
	r.forget("roxana")
	if st := r.snapshot(t0); !reflect.DeepEqual(st.Present, []string{"ana"}) {
		t.Errorf("after one person left = %+v, want ana alone", st)
	}
}

// The flip is announced once, to somebody listening. snapshot must not eat it.
func TestOnlyAssessConsumesTheFlip(t *testing.T) {
	r := newRoom("", time.Hour)
	r.heardOne(heard("roxana", false, viaMatch), true, t0)
	if st := r.snapshot(t0); st.Changed {
		t.Error("snapshot consumed the transition")
	}
	if st := r.assess(t0); !st.Changed || !st.Shared {
		t.Errorf("assess = %+v, want the shared flip", st)
	}
	if st := r.assess(t0); st.Changed {
		t.Error("the same flip was announced twice")
	}
	// And back again once everyone has aged out.
	if st := r.assess(t0.Add(2 * time.Hour)); !st.Changed || st.Shared {
		t.Errorf("assess after the visit = %+v, want the private flip", st)
	}
}

func TestPresentIsSortedForStableLogs(t *testing.T) {
	r := newRoom("", time.Hour)
	r.declare(true, []string{"roxana", "ana", "beto"}, t0)
	want := []string{"ana", "beto", "roxana"}
	for i := 0; i < 5; i++ {
		if got := r.snapshot(t0).Present; !reflect.DeepEqual(got, want) {
			t.Fatalf("present = %v, want %v", got, want)
		}
	}
}

// A nil room is the feature switched off, and every call site relies on it
// answering rather than panicking.
func TestNilRoomIsAPrivateRoom(t *testing.T) {
	var r *room
	r.heardOne(heard("roxana", false, viaMatch), true, t0)
	r.declare(true, []string{"roxana"}, t0)
	r.forget("roxana")
	if st := r.snapshot(t0); st.Shared {
		t.Error("a nil room reported company")
	}
	if st := r.assess(t0); st.Shared || st.Changed {
		t.Error("a nil room reported a transition")
	}
}

func TestNewRoomFallsBackToTheDefaultTimeout(t *testing.T) {
	if got := newRoom("", 0).timeout; got != defaultRoomTimeoutMinutes*time.Minute {
		t.Errorf("timeout = %v, want the default", got)
	}
}

// With company present everyone talks in one session: splitting the room by
// voice would leave the agent answering each person without what the other
// just said.
func TestSessionForCollapsesTheRoomIntoOneThread(t *testing.T) {
	owner := heard("nicolas", true, viaMatch)
	guest := heard("roxana", false, viaMatch)
	room := sessionKey + ":" + roomSessionSlug

	if got := sessionFor(owner, true); got != room {
		t.Errorf("owner in company = %q, want %q", got, room)
	}
	if got := sessionFor(guest, true); got != room {
		t.Errorf("guest in company = %q, want %q", got, room)
	}
	// Alone, nothing changes from before the room existed.
	if got := sessionFor(owner, false); got != sessionKey {
		t.Errorf("owner alone = %q, want %q", got, sessionKey)
	}
	if got := sessionFor(guest, false); got != sessionKey+":roxana" {
		t.Errorf("guest alone = %q", got)
	}
}

func TestRoomStateLabelAndAudience(t *testing.T) {
	shared := roomState{Shared: true}
	private := roomState{}
	if shared.label() != "shared" || private.label() != "private" {
		t.Errorf("labels = %q, %q", shared.label(), private.label())
	}
	if shared.audience() != tools.AudienceShared {
		t.Errorf("shared audience = %q", shared.audience())
	}
	if private.audience() != "" {
		t.Errorf("private audience = %q, want blank", private.audience())
	}
}

// The spoken line names the change, never the people: reciting who it thinks
// is in the room is wrong the moment identification is off by one.
func TestRoomChangeLineNamesNobody(t *testing.T) {
	for _, lang := range []string{"en", "es"} {
		for _, st := range []roomState{{Shared: true}, {}} {
			line := roomChangeLine(st, lang)
			if line == "" {
				t.Fatalf("no line for %v in %s", st.Shared, lang)
			}
			if containsAny(line, "roxana", "someone", "Roxana") {
				t.Errorf("the transition line names people: %q", line)
			}
		}
	}
	if roomChangeLine(roomState{Shared: true}, "es") == roomChangeLine(roomState{Shared: true}, "en") {
		t.Error("the Spanish line is the English one")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// One recording holding several distinct voices has at most one owner in it,
// so everybody past the first is somebody else. That holds even when none of
// them could be named — the case that matters most is the owner and a guest
// whose only contribution is too brief to identify.
func TestSeveralVoicesInOneRecordingAreCompany(t *testing.T) {
	cases := []struct {
		name   string
		voices []speakerIdentity
	}{
		{"the owner and a voice nobody could name", []speakerIdentity{
			heard("nicolas", true, viaMatch), {via: viaShort},
		}},
		{"two voices, neither of them readable", []speakerIdentity{
			{via: viaShort}, {via: viaShort},
		}},
		{"the owner and a named guest", []speakerIdentity{
			heard("nicolas", true, viaMatch), heard("roxana", false, viaMatch),
		}},
	}
	for _, tc := range cases {
		r := newRoom("", time.Hour)
		r.heard(tc.voices, true, t0)
		if st := r.snapshot(t0); !st.Shared {
			t.Errorf("%s: room = %+v, want shared", tc.name, st)
		}
	}
}

// The same arithmetic must not fire on one voice, however it was read: a
// single speaker is the baseline, not a crowd.
func TestOneVoiceIsNeverCompanyByArithmetic(t *testing.T) {
	for _, who := range []speakerIdentity{
		heard("nicolas", true, viaMatch), {via: viaShort}, {via: viaUnavailable},
		// A reading the identifier would not commit to is not a stranger.
		// Inventing an occupant out of one would make every brief reply of
		// the owner's read as somebody new in the room.
		{via: viaAmbiguous}, {via: viaUnsure},
	} {
		r := newRoom("", time.Hour)
		r.heard([]speakerIdentity{who}, true, t0)
		if st := r.snapshot(t0); st.Shared {
			t.Errorf("via %s alone filled the room: %+v", who.via, st)
		}
	}
}

// A restart is not a departure. A config change and an upgrade both re-exec
// the gateway, and the guest is still on the sofa: coming back up private is
// the one error this tracker exists not to make, and it is the expensive
// direction — a private answer spoken into a room with somebody else in it.
func TestRoomSurvivesARestart(t *testing.T) {
	home := t.TempDir()
	before := newRoom(home, time.Hour)
	before.heard([]speakerIdentity{heard("roxana", false, viaMatch)}, true, time.Now())
	if st := before.assess(time.Now()); !st.Shared || !st.Changed {
		t.Fatalf("the room did not go shared in the first place: %+v", st)
	}

	after := newRoom(home, time.Hour)
	st := after.assess(time.Now())
	if !st.Shared {
		t.Errorf("the room came back private with a guest still in it: %+v", st)
	}
	if !reflect.DeepEqual(st.Present, []string{"roxana"}) {
		t.Errorf("present = %v, want the guest who was here", st.Present)
	}
	// The user was already told. Saying it again on every config reload is
	// how a useful announcement becomes noise.
	if st.Changed {
		t.Error("the restart re-announced company the user had already been told about")
	}
}

// The timeout is measured against the wall clock, not against uptime: an
// occupant whose last word was longer ago than the timeout has left, whether
// or not the process was running to notice.
func TestRoomForgetsOccupantsWhoAgedOutWhileItWasDown(t *testing.T) {
	home := t.TempDir()
	// The guest last spoke an hour ago, and the timeout is ten minutes: they
	// left, and the process was not running to see it.
	longAgo := time.Now().Add(-time.Hour)
	before := newRoom(home, 10*time.Minute)
	before.heard([]speakerIdentity{heard("roxana", false, viaMatch)}, true, longAgo)
	before.assess(longAgo)

	after := newRoom(home, 10*time.Minute)
	st := after.assess(time.Now())
	if st.Shared {
		t.Errorf("a guest who left during the downtime came back: %+v", st)
	}
	// They were announced as company before, so the return to private is
	// owed out loud.
	if !st.Changed {
		t.Error("the return to an empty room went unannounced")
	}
}

// A room whose file is unreadable must not take the channel down with it, or
// a corrupt scratch file would cost the user their microphone.
func TestRoomSurvivesAnUnreadableFile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, roomFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := newRoom(home, time.Hour).snapshot(t0); st.Shared {
		t.Errorf("an unreadable room file produced company: %+v", st)
	}
}

// A room with nowhere to write is the shape the tests and a homeless config
// take: it must work, and simply not outlive the process.
func TestRoomWithoutAHomeStillTracks(t *testing.T) {
	r := newRoom("", time.Hour)
	r.heard([]speakerIdentity{heard("roxana", false, viaMatch)}, true, t0)
	if st := r.snapshot(t0); !st.Shared {
		t.Errorf("a room with no file on disk stopped tracking: %+v", st)
	}
}
