package voice

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Room occupancy: who is within earshot, and so who the reply is spoken to.
//
// Speaker identification answers "who said this". The room answers "who can
// hear the answer" — a different question, and the one confidentiality turns
// on. Giving a guest their own session keeps their dialogue theirs, but it
// cannot touch the leak that actually matters: the owner asking something
// private while somebody else is standing there. That turn is the owner's on
// every axis except the only one that counts, and it is answered out loud
// into a room with another person in it.
//
// Presence is inferred from sound, which is what makes this tracker
// deliberately asymmetric. One utterance in a voice that is not the owner's
// declares company; only a long silence, or the user saying so, takes it
// back. The costs are not symmetric either: a room wrongly called shared
// costs a coy answer the user corrects in one sentence, and a room wrongly
// called private says a secret to a guest, which nothing corrects.
//
// The blind spot is honest and irreducible here: somebody who walks in and
// never makes a sound is invisible to every acoustic signal. That is what
// declare exists for.

const (
	// roomUnknownOccupant stands for a voice no profile matched. It counts as
	// company only because the owner's voice *would* have matched: somebody
	// is here who is not the person this machine belongs to.
	roomUnknownOccupant = "someone"

	// roomSessionSlug is the session a shared room talks in. It is reserved
	// against speaker names, since a guest renamed "Room" would otherwise
	// slug into this very key and inherit the room's whole conversation.
	roomSessionSlug = "room"

	// defaultRoomTimeoutMinutes is how long an occupant is presumed present
	// after their last word. It is measured in the length of a visit, not the
	// length of a pause: people sit quietly, and a short timeout would hand
	// the room back to private while the guest is still on the sofa.
	defaultRoomTimeoutMinutes = 30

	// roomFile is where the room outlives the process, beside the voice
	// profiles. A restart is not a departure: a config change and an upgrade
	// both re-exec the gateway, and coming back up private while the guest is
	// still on the sofa is the one error this tracker is built not to make.
	// Holding it in memory alone meant the timeout that exists because people
	// sit quietly was thrown away by the most ordinary event on the machine.
	roomFile = "voice-room.json"
)

// roomState is what the room is, for the log line, the health endpoint, and
// the turn about to run.
type roomState struct {
	// Shared reports that somebody besides the owner is present.
	Shared bool
	// Present names them, sorted, so a log line reads the same every time.
	Present []string
	// Changed marks the assessment that flipped the state, so the flip is
	// announced once instead of on every utterance after it.
	Changed bool
}

// room tracks the voices believed present besides the owner's. A nil *room is
// the feature switched off and answers every question as an empty private
// room, so call sites need no guard.
type room struct {
	mu        sync.Mutex
	timeout   time.Duration
	path      string
	occupants map[string]time.Time
	// wasShared is what the last assessment reported.
	wasShared bool
}

// roomOnDisk is the file's shape. Shared is what was last said out loud, not
// what the occupants add up to now: restoring it is what keeps a restart from
// re-announcing company the user was already told about, while still leaving
// the "we're on our own again" line owed when the guest aged out meanwhile.
type roomOnDisk struct {
	Occupants map[string]time.Time `json:"occupants"`
	Shared    bool                 `json:"shared"`
}

// newRoom loads the room under home. A blank home is a room that lives only
// as long as the process — the shape the tests and a nil-config path want.
func newRoom(home string, timeout time.Duration) *room {
	if timeout <= 0 {
		timeout = defaultRoomTimeoutMinutes * time.Minute
	}
	r := &room{timeout: timeout, occupants: map[string]time.Time{}}
	if home == "" {
		return r
	}
	r.path = filepath.Join(home, roomFile)
	r.load(time.Now())
	return r
}

// load restores the room from disk, aged against now: an occupant whose last
// word was longer ago than the timeout has left, whether the process was
// running to notice or not. A file beyond reading leaves an empty room, which
// is the state the process would have come up in anyway.
func (r *room) load(now time.Time) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var disk roomOnDisk
	if err := json.Unmarshal(raw, &disk); err != nil {
		slog.Warn("the saved room could not be read; starting with an empty one", "error", err)
		return
	}
	for name, at := range disk.Occupants {
		if name != "" && now.Sub(at) <= r.timeout {
			r.occupants[name] = at
		}
	}
	r.wasShared = disk.Shared
	if len(r.occupants) > 0 {
		slog.Info("the room came back as it was left", "present", len(r.occupants))
	}
}

// saveLocked writes the room out. A write that fails costs the next restart
// its memory of who was here, not this conversation.
func (r *room) saveLocked() {
	if r.path == "" {
		return
	}
	raw, err := json.MarshalIndent(
		roomOnDisk{Occupants: r.occupants, Shared: r.wasShared}, "", "  ")
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.path)
}

// occupantFor names the occupant one identification is evidence of, and
// whether that evidence is strong enough to create an entry or may only
// refresh one. Only a fresh reading of this utterance's own audio creates.
func occupantFor(who speakerIdentity, hasProfiles bool) (name string, refreshOnly bool) {
	if who.inherited {
		// A name carried over from the last utterance is not evidence that
		// somebody new is here. It is evidence that somebody already here
		// still is: a guest whose every follow-up is "mhm" is never embedded
		// and would otherwise age out of a room they never left.
		if who.name != "" && !who.primary {
			return who.name, true
		}
		return "", false
	}
	switch who.via {
	case viaMatch, viaEnrolled:
		if who.primary {
			return "", false // the owner is the baseline, never an occupant
		}
		return who.name, false
	case viaUnknown:
		// A voice that matched nothing is company only where the owner's
		// voice would have matched. With nobody enrolled there is no
		// baseline, so an unmatched voice says nothing about who is here —
		// it is the machine's first voice, not its second.
		if hasProfiles {
			return roomUnknownOccupant, false
		}
	}
	// viaOverlap, viaShort, viaAmbiguous and viaUnsure arrive here only when
	// the sticky window was empty, and viaUnavailable means the room could
	// not be read at all. None of them may invent an occupant: a reading the
	// identifier would not commit to says nothing about who is here, and an
	// embedding outage means the microphone went blind, not that the room
	// filled up.
	return "", false
}

// heard folds one utterance's voices into the room.
func (r *room) heard(voices []speakerIdentity, hasProfiles bool, now time.Time) {
	if r == nil || len(voices) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	recorded, changed := 0, false
	for _, who := range voices {
		name, refreshOnly := occupantFor(who, hasProfiles)
		if name == "" {
			continue
		}
		if _, known := r.occupants[name]; refreshOnly && !known {
			continue
		}
		r.occupants[name] = now
		recorded++
		changed = true
	}
	// Arithmetic, not acoustics. One recording holding several distinct
	// voices has at most one owner in it, so everybody past the first is
	// somebody else — which holds even when none of them could be named, and
	// even before anyone is enrolled at all. Without this the case that
	// matters most reads as an empty room: the owner and a guest talking, the
	// guest too brief to identify, and a private answer spoken over them
	// both.
	if len(voices) > 1 && recorded < len(voices)-1 {
		r.occupants[roomUnknownOccupant] = now
		changed = true
	}
	// Only when somebody was actually recorded: an empty room hearing its
	// owner is most of the utterances on this machine, and none of them are
	// news worth a write.
	if changed {
		r.saveLocked()
	}
}

// declare is the user's own word on the room, which outranks the microphone.
// It is the only signal that can reach the two things sound cannot say:
// somebody who came in without speaking, and somebody who left — departure is
// announced by nothing at all, so without this the room only ever times out.
func (r *room) declare(shared bool, names []string, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.saveLocked()
	if !shared {
		clear(r.occupants)
		return
	}
	named := false
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			r.occupants[n] = now
			named = true
		}
	}
	if !named {
		r.occupants[roomUnknownOccupant] = now
	}
}

// forget drops one occupant by name — the person who just left, where the
// user says so and the rest of the room stays.
func (r *room) forget(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.occupants, strings.TrimSpace(name))
	r.saveLocked()
}

// snapshot reports the room without latching a change, for readers that must
// not consume the announcement the next turn owes the user.
func (r *room) snapshot(now time.Time) roomState {
	if r == nil {
		return roomState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked(now)
}

// assess reports the room and whether this is the assessment that flipped it.
// Only the utterance path may call it: Changed is consumed, not observed.
func (r *room) assess(now time.Time) roomState {
	if r == nil {
		return roomState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stateLocked(now)
	st.Changed = st.Shared != r.wasShared
	r.wasShared = st.Shared
	// Once per turn, and it carries both halves: who is here, and what they
	// have already been told about it.
	r.saveLocked()
	return st
}

func (r *room) stateLocked(now time.Time) roomState {
	for name, at := range r.occupants {
		if now.Sub(at) > r.timeout {
			delete(r.occupants, name)
		}
	}
	present := make([]string, 0, len(r.occupants))
	for name := range r.occupants {
		present = append(present, name)
	}
	sort.Strings(present)
	return roomState{Shared: len(present) > 0, Present: present}
}

// sessionFor is where this turn's conversation lives. With company present
// there is one conversation rather than one per voice: the people in the room
// are talking to Factor together, and splitting them would leave it answering
// each person without what the other just said. Attribution still names who
// spoke, so the model can tell them apart inside that one thread — while the
// owner's private session, which holds everything said alone, stays out of a
// context the room can hear.
func sessionFor(who speakerIdentity, shared bool) string {
	if shared {
		return sessionKey + ":" + roomSessionSlug
	}
	return who.session()
}

// label is how the room reads in a log line and on the health endpoint.
func (st roomState) label() string {
	if st.Shared {
		return "shared"
	}
	return "private"
}

// audience is what the turn tells the agent loop, and through it the memory
// scope: blank is the ordinary private conversation.
func (st roomState) audience() string {
	if st.Shared {
		return tools.AudienceShared
	}
	return ""
}

// roomChangeLine is what the agent says out loud when the room flips. It
// names the change rather than the people: reciting who it thinks is present
// would be both creepy and wrong the moment identification is off by one.
func roomChangeLine(st roomState, language string) string {
	if isSpanish(language) {
		if st.Shared {
			return "Ahora no estamos solos, así que voy a ser discreto."
		}
		return "Volvemos a estar solos."
	}
	if st.Shared {
		return "We're not alone now, so I'll keep things general."
	}
	return "We're on our own again."
}
