package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

// The scenario the feature exists for: the owner talks alone, a second voice
// arrives, and from that moment the conversation runs in the room's own
// session under a shared audience — so the memory scope behind it can no
// longer reach what was said in private.
func TestASecondVoiceMovesTheConversationIntoTheRoom(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.enableRoom(time.Hour)
	h.start()

	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	alone := h.turn(10 * time.Second)
	if alone.session != sessionKey || alone.audience != "" {
		t.Fatalf("alone: session %q audience %q, want the private session", alone.session, alone.audience)
	}
	h.waitQuiet()

	// A guest speaks. Everything from here is audible to them.
	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	guest := h.turn(10 * time.Second)
	if guest.session != sessionKey+":"+roomSessionSlug {
		t.Errorf("the guest's turn ran in %q, want the room session", guest.session)
	}
	if guest.audience != tools.AudienceShared {
		t.Errorf("the guest's turn ran with audience %q, want shared", guest.audience)
	}
	if guest.speaker != "speaker-2" {
		t.Errorf("the guest went unattributed: %q", guest.speaker)
	}
	h.waitQuiet()

	// And the owner's own next turn is in the room too — this is the leak a
	// per-speaker split cannot close, because the turn is entirely theirs.
	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	withCompany := h.turn(10 * time.Second)
	if withCompany.session != sessionKey+":"+roomSessionSlug {
		t.Errorf("the owner's turn with company ran in %q", withCompany.session)
	}
	if withCompany.audience != tools.AudienceShared {
		t.Errorf("the owner's turn with company ran with audience %q, want shared", withCompany.audience)
	}
}

// The user is told once, before the answer that depends on it. A silent
// switch is untrustworthy in both directions.
func TestTheRoomFlipIsSpokenOnce(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.enableRoom(time.Hour)
	h.start()

	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	h.turn(10 * time.Second)
	h.waitQuiet()

	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	h.turn(10 * time.Second)
	h.waitQuiet()
	announcement := roomChangeLine(roomState{Shared: true}, "")
	if !h.spoke(announcement) {
		t.Fatalf("the flip was never announced; said: %v", h.synthesized())
	}

	// A second guest turn is the same room: saying it again would be noise.
	h.say()
	h.turn(10 * time.Second)
	h.waitQuiet()
	said := 0
	for _, line := range h.synthesized() {
		if line == announcement {
			said++
		}
	}
	if said != 1 {
		t.Errorf("the flip was announced %d times, want once", said)
	}
}

// Speech the gate turned away never becomes a turn, but it still says who is
// in the room — which is what covers the guest who is talking to the user
// rather than to Factor. Push-to-talk is the cleanest gate to prove it
// through: nothing is ever accepted unless it was armed for, so an utterance
// reaching the room can only have come from the presence path.
func TestUnaddressedSpeechFillsTheRoomWithoutRunningATurn(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = activationPTT })
	h.enableSpeakerID(unknownEnroll)
	h.enableRoom(time.Hour)
	// The owner is already known; this machine has heard them before.
	h.v.speakers.enroll([]float64{1, 0, 0})
	h.start()

	// A guest says something to the user. Nothing is armed: no turn.
	h.setTranscript("did you see the game last night")
	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	h.noTurn(2 * time.Second)
	waitUntil(t, func() bool { return h.v.room.snapshot(time.Now()).Shared })

	// The very next thing the owner asks is already answered to the room.
	h.setTranscript("what is on my calendar")
	h.setEmbedding([]float64{1, 0, 0})
	h.v.ArmPTT()
	h.say()
	call := h.turn(10 * time.Second)
	if call.audience != tools.AudienceShared {
		t.Errorf("audience = %q, want shared before the guest ever addressed Factor", call.audience)
	}
	if call.session != sessionKey+":"+roomSessionSlug {
		t.Errorf("session = %q, want the room session", call.session)
	}
}

// Reading the room must not teach it: ambient chatter would otherwise mint
// profiles from a television and steal the name a short follow-up inherits.
func TestPresenceReadingHasNoSideEffects(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = activationPTT })
	h.enableSpeakerID(unknownEnroll)
	h.enableRoom(time.Hour)
	h.v.speakers.enroll([]float64{1, 0, 0})
	h.start()

	// One real turn, so there is a sticky speaker worth stealing.
	h.setEmbedding([]float64{1, 0, 0})
	h.v.ArmPTT()
	h.say()
	h.turn(10 * time.Second)
	h.waitQuiet()

	before := len(h.v.speakers.list())
	lastBefore := h.v.lastSpeakerName()
	if lastBefore == "" {
		t.Fatal("the owner's turn left no sticky speaker to protect")
	}

	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	h.noTurn(2 * time.Second)
	waitUntil(t, func() bool { return h.v.room.snapshot(time.Now()).Shared })

	if after := len(h.v.speakers.list()); after != before {
		t.Errorf("ambient speech enrolled a profile: %d → %d", before, after)
	}
	if got := h.v.lastSpeakerName(); got != lastBefore {
		t.Errorf("ambient speech moved the sticky speaker: %q → %q", lastBefore, got)
	}
}

// With the feature off, everything behaves exactly as it did before it existed.
func TestRoomOffKeepsPerSpeakerSessions(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.start()

	h.setEmbedding([]float64{1, 0, 0})
	h.say()
	h.turn(10 * time.Second)
	h.waitQuiet()

	h.setEmbedding([]float64{0, 1, 0})
	h.say()
	guest := h.turn(10 * time.Second)
	if guest.session != sessionKey+":speaker-2" || guest.audience != "" {
		t.Errorf("with the room off: session %q audience %q", guest.session, guest.audience)
	}
}

func TestHealthAndStatusReportTheRoom(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.enableRoom(time.Hour)
	h.start()

	read := func() map[string]any {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", h.v.cfg.ControlPort))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	if got := read()["room"]; got != "private" {
		t.Errorf("room = %v, want private", got)
	}
	h.v.room.declare(true, []string{"roxana"}, time.Now())
	body := read()
	if body["room"] != "shared" {
		t.Errorf("room = %v, want shared", body["room"])
	}
	present, _ := body["room_present"].([]any)
	if len(present) != 1 || present[0] != "roxana" {
		t.Errorf("room_present = %v", body["room_present"])
	}

	// Reading the endpoint must not eat the announcement the next turn owes.
	if st := h.v.room.assess(time.Now()); !st.Changed {
		t.Error("the health endpoint consumed the pending transition")
	}
}

// The feature is off unless the machine can tell one voice from another, and
// asking for it anyway is refused rather than silently ignored — a privacy
// setting that quietly does nothing is worse than one that errors.
func TestRoomIsolationNeedsSpeakerID(t *testing.T) {
	on, off := true, false

	cfg := validConfig()
	cfg.RoomIsolation = &on
	cfg.applyDefaults()
	if err := cfg.validate(); err == nil {
		t.Error("room_isolation without speaker_id was accepted")
	}

	if (&Config{}).roomIsolation() {
		t.Error("room isolation is on without speaker_id")
	}
	if !(&Config{SpeakerID: true}).roomIsolation() {
		t.Error("speaker_id alone should bring room isolation with it")
	}
	if (&Config{SpeakerID: true, RoomIsolation: &off}).roomIsolation() {
		t.Error("an explicit off was ignored")
	}
}

func TestRoomTimeoutDefaults(t *testing.T) {
	cfg := validConfig()
	cfg.applyDefaults()
	if cfg.RoomTimeoutMinutes != defaultRoomTimeoutMinutes {
		t.Errorf("room_timeout_minutes = %d, want %d", cfg.RoomTimeoutMinutes, defaultRoomTimeoutMinutes)
	}
}

// A speaker renamed into the room's slug would inherit the whole room's
// conversation.
func TestRoomSlugIsReservedAgainstSpeakerNames(t *testing.T) {
	s := newSpeakerStore(t.TempDir())
	name := s.enroll([]float64{1, 0})
	if err := s.rename(name, "Room"); err == nil {
		t.Error("a speaker was renamed into the room session")
	}
	if err := s.rename(name, "Roxana"); err != nil {
		t.Errorf("an ordinary rename was refused: %v", err)
	}
}

func TestRoomToolDrivesTheRoom(t *testing.T) {
	h := newVoiceHarness(t, nil)
	h.enableSpeakerID(unknownEnroll)
	h.enableRoom(time.Hour)
	tool := &roomTool{voice: h.v}

	res := tool.Execute(context.Background(), map[string]any{"action": "status"})
	if res.IsError || !strings.Contains(res.ForLLM, "private") {
		t.Errorf("status on an empty room = %+v", res)
	}

	res = tool.Execute(context.Background(), map[string]any{
		"action": "company", "names": []any{"Roxana", 7, "  "},
	})
	if res.IsError || !strings.Contains(res.ForLLM, "Roxana") {
		t.Errorf("company = %+v", res)
	}
	// A shared reading must say how the user's word corrects it: the
	// microphone hears arrivals, never departures.
	if !strings.Contains(res.ForLLM, "action=alone") {
		t.Errorf("a shared room never offered the correction: %+v", res)
	}

	res = tool.Execute(context.Background(), map[string]any{"action": "left", "names": []any{"Roxana"}})
	if res.IsError || !strings.Contains(res.ForLLM, "private") {
		t.Errorf("left = %+v", res)
	}

	if res := tool.Execute(context.Background(), map[string]any{"action": "left"}); !res.IsError {
		t.Error("left with nobody named was accepted")
	}

	tool.Execute(context.Background(), map[string]any{"action": "company"})
	res = tool.Execute(context.Background(), map[string]any{"action": "alone"})
	if res.IsError || !strings.Contains(res.ForLLM, "private") {
		t.Errorf("alone = %+v", res)
	}

	if res := tool.Execute(context.Background(), map[string]any{"action": "sideways"}); !res.IsError {
		t.Error("an unknown action was accepted")
	}

	off := &roomTool{voice: &Voice{}}
	if res := off.Execute(context.Background(), map[string]any{"action": "status"}); !res.IsError {
		t.Error("the tool worked with room isolation off")
	}
}

func TestRoomToolIsOfferedOnlyWithTheRoomOn(t *testing.T) {
	h := newVoiceHarness(t, nil)
	if hasTool(h.v.Toolset(), "room") {
		t.Error("the room tool was offered with the feature off")
	}
	h.enableRoom(time.Hour)
	if !hasTool(h.v.Toolset(), "room") {
		t.Error("the room tool is missing with the feature on")
	}
}

func hasTool(set []tools.Tool, name string) bool {
	for _, tool := range set {
		if tool.Name() == name {
			return true
		}
	}
	return false
}

func TestStatusLineSaysWhenTheRoomIsShared(t *testing.T) {
	shared := Status{Configured: true, Enabled: true, Tier: "local", Activation: "always",
		Listening: true, Room: "shared"}
	if !strings.Contains(shared.Line(), "room shared") {
		t.Errorf("status line = %q", shared.Line())
	}
	private := shared
	private.Room = "private"
	if strings.Contains(private.Line(), "room") {
		t.Errorf("a private room was announced: %q", private.Line())
	}
}
