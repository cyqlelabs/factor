package voice

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Speaker identification: which of the household's voices one utterance is.
// The managed speech server computes an embedding of the utterance's audio;
// the profile store names it. A known voice that is not the machine's owner
// holds its own conversation — its own session key — so each person's thread
// of dialogue stays theirs. (Session history separates; ambient memory is
// still one shared graph, so a fact one person tells Factor can surface in
// another's recall.)

const (
	// speakerStickyWindow is how long a short or overlapped utterance inherits
	// the last identified voice: a "yes" mid-conversation belongs to whoever
	// was just talking, and half a second of audio cannot say who that is.
	speakerStickyWindow = 30 * time.Second

	// speakerMinBytes is the least speech worth embedding — below about a
	// second the vectors are noise, and naming the wrong person costs more
	// than naming nobody. Judged over the utterance minus its silence tail,
	// which is what gets embedded.
	speakerMinBytes = captureRate * 2 // one second of 16 kHz s16le

	// speakerLearnMargin is how far above the threshold a match has to sit
	// before it is folded into the profile. Matching and learning are
	// different bets: a borderline match costs one misattributed sentence,
	// while borderline audio folded into a centroid moves it — toward the
	// room, toward whoever else was talking — and every later match is judged
	// against the moved one. That is how profiles drift into each other, and
	// it is what "it stopped recognizing me" looks like from the inside.
	speakerLearnMargin = 0.15
)

// How one identity was arrived at. It rides the decision purely so the log
// can say it: "it answered as the wrong person" is only diagnosable if the
// line names which branch ran and how close the vectors were.
const (
	viaMatch       = "match"       // a profile above the threshold
	viaEnrolled    = "enrolled"    // nobody matched, so a profile was created
	viaUnknown     = "unknown"     // nobody matched, and the policy is anonymous
	viaOverlap     = "overlap"     // recorded with the agent's voice in it: not embedded
	viaShort       = "short"       // too little speech to identify: not embedded
	viaUnavailable = "unavailable" // the embedding could not be computed
)

// speakerIdentity is what the channel decided about who spoke. A blank name
// is a voice it will not commit to; primary is the machine's owner, who keeps
// the channel's original session.
type speakerIdentity struct {
	name    string
	primary bool

	// via and similarity are the decision's own account of itself, for the
	// log: which branch named this voice, and how close the closest profile
	// was — the number speaker_threshold is tuned against.
	via        string
	similarity float64
	// inherited marks a name taken from the conversation rather than from
	// this utterance's own audio.
	inherited bool
}

// identifySpeaker names the voice behind one accepted utterance. Overlapped
// and too-short utterances are never embedded — the first carries the
// speakers' own sound mixed in, the second not enough voice to be anyone
// reliably — they inherit the conversation's current speaker instead.
func (v *Voice) identifySpeaker(ctx context.Context, utterance capturedUtterance) speakerIdentity {
	if v.speakers == nil {
		return speakerIdentity{}
	}
	if utterance.overlapped {
		return v.stickySpeaker(viaOverlap)
	}
	voiced := v.voiced(utterance.pcm)
	if len(voiced) < speakerMinBytes {
		return v.stickySpeaker(viaShort)
	}
	embedding, err := v.speechClient().embed(ctx, voiced)
	if err != nil || len(embedding) == 0 {
		// One warning per outage: with the managed server down or serving
		// without a speaker model, every utterance would repeat it.
		if err != nil && ctx.Err() == nil && !v.embedFailing.Swap(true) {
			slog.Warn("speaker identification failed; not naming speakers until it recovers",
				"error", v.redact(err))
		}
		return v.stickySpeaker(viaUnavailable)
	}
	if v.embedFailing.Swap(false) {
		slog.Info("speaker identification recovered")
	}
	name, similarity := v.speakers.match(embedding)
	// Every profile's score, for tuning speaker_threshold against real voices
	// — the one question the decision alone cannot answer is how close the
	// runners-up were. Built only when the log would print it.
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.Debug("speaker scores", "scores", v.speakers.scores(embedding),
			"threshold", v.cfg.SpeakerThreshold)
	}
	if name != "" && similarity >= v.cfg.SpeakerThreshold {
		if similarity >= v.cfg.SpeakerThreshold+speakerLearnMargin {
			v.speakers.learn(name, embedding)
		}
		return v.rememberSpeaker(name, viaMatch, similarity)
	}
	if v.cfg.UnknownSpeaker == unknownEnroll {
		enrolled := v.speakers.enroll(embedding)
		slog.Info("a new voice enrolled", "speaker", enrolled,
			"closest", name, "similarity", round2(similarity), "threshold", v.cfg.SpeakerThreshold)
		return v.rememberSpeaker(enrolled, viaEnrolled, similarity)
	}
	return v.rememberSpeaker("", viaUnknown, similarity)
}

// presenceOf reads the voice behind an utterance the activation gate turned
// away — speech in the room that was not addressed to Factor. It answers only
// the room's question, and it answers it read-only: it must not enroll a
// profile, must not fold the audio into one, and must not move the sticky
// speaker. Ambient conversation is the wrong teacher for all three — a
// television would mint profiles, and a sentence spoken across the room would
// steal the name a following "yes" inherits.
func (v *Voice) presenceOf(ctx context.Context, utterance capturedUtterance) speakerIdentity {
	if v.speakers == nil {
		return speakerIdentity{}
	}
	if utterance.overlapped {
		return speakerIdentity{via: viaOverlap}
	}
	voiced := v.voiced(utterance.pcm)
	if len(voiced) < speakerMinBytes {
		return speakerIdentity{via: viaShort}
	}
	embedding, err := v.speechClient().embed(ctx, voiced)
	if err != nil || len(embedding) == 0 {
		return speakerIdentity{via: viaUnavailable}
	}
	name, similarity := v.speakers.match(embedding)
	if name != "" && similarity >= v.cfg.SpeakerThreshold {
		return speakerIdentity{name: name, primary: v.speakers.isPrimary(name),
			via: viaMatch, similarity: similarity}
	}
	// Nobody matched. Unlike the addressed path this never enrolls, so the
	// answer is the same under either unknown_speaker setting: somebody is
	// here who is not the owner.
	return speakerIdentity{via: viaUnknown, similarity: similarity}
}

// voiced is the utterance without the closing silence every segment carries:
// silence_ms of quiet is what ends one, so it is always there, and against a
// three-second sentence it is most of the recording. It says nothing about
// whose voice this is, and an embedding computed over it is partly the room's
// — which is what pulls two people's profiles toward each other until neither
// is recognizable.
func (v *Voice) voiced(pcm []byte) []byte {
	tail := v.cfg.SilenceMs * captureRate * 2 / 1000
	end := len(pcm) - tail
	if end &^= 1; end <= 0 { // sample-aligned, or the vector is byte-swapped noise
		return nil
	}
	return pcm[:end]
}

// speakerProfilesExist reports whether any voice is enrolled, which is what
// makes an unmatched voice mean "not the owner" rather than "the first voice".
func (v *Voice) speakerProfilesExist() bool {
	return v.speakers != nil && v.speakers.hasProfiles()
}

// stickySpeaker is the conversation's current voice, while it is current.
func (v *Voice) stickySpeaker(via string) speakerIdentity {
	v.mu.Lock()
	name, at := v.lastSpeaker, v.lastSpeakerAt
	v.mu.Unlock()
	if name == "" || time.Since(at) > speakerStickyWindow {
		return speakerIdentity{via: via}
	}
	who := v.identity(name)
	who.via, who.inherited = via, true
	return who
}

// rememberSpeaker records who is talking now — including nobody, so an
// unknown voice does not ride the previous speaker's identity.
func (v *Voice) rememberSpeaker(name, via string, similarity float64) speakerIdentity {
	v.mu.Lock()
	v.lastSpeaker, v.lastSpeakerAt = name, time.Now()
	v.mu.Unlock()
	who := v.identity(name)
	who.via, who.similarity = via, similarity
	return who
}

// lastSpeakerName is who spoke last, for the health endpoint and the
// speakers tool — "that was Roxana" needs to know which voice "that" was.
func (v *Voice) lastSpeakerName() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.lastSpeaker
}

// renameSticky follows a profile rename, so a short follow-up utterance does
// not resurrect the old name.
func (v *Voice) renameSticky(from, to string) {
	v.mu.Lock()
	if v.lastSpeaker == from {
		v.lastSpeaker = to
	}
	v.mu.Unlock()
}

func (v *Voice) identity(name string) speakerIdentity {
	if name == "" {
		return speakerIdentity{}
	}
	return speakerIdentity{name: name, primary: v.speakers.isPrimary(name)}
}

// session is where this identity's conversation lives, and attributed is the
// name that travels with the turn: a named guest speaks under their own key
// and their words are attributed to them, while the owner and the unnamed
// keep the channel's original session and go unattributed, exactly as before
// identification existed — a machine with one voice must not start narrating
// whose turn it is.
func (who speakerIdentity) session() string {
	if who.name == "" || who.primary {
		return sessionKey
	}
	return sessionKey + ":" + speakerSlug(who.name)
}

func (who speakerIdentity) attributed() string {
	if who.primary {
		return ""
	}
	return who.name
}

// logFields is how a decision explains itself on the "voice heard" line: who
// it settled on, which branch decided that, how close the closest profile
// was, and the session the turn will run in — the four answers behind "why
// did it answer as the wrong person", and behind "why did it answer as
// nobody" when the threshold is the reason.
func (who speakerIdentity) logFields(session string, addressed bool) []any {
	name := who.name
	if name == "" {
		name = "unnamed"
	}
	fields := []any{"speaker", name, "via", who.via}
	switch who.via {
	case viaMatch, viaEnrolled, viaUnknown:
		fields = append(fields, "similarity", round2(who.similarity))
	}
	if who.inherited {
		fields = append(fields, "inherited", true)
	}
	// An utterance the gate turned away has no session — it was read for the
	// room and for nothing else. Naming one would suggest a turn ran.
	if addressed {
		fields = append(fields, "session", session)
	}
	return fields
}

// round2 keeps a similarity readable in a log line: 0.83, not 0.8271604938.
func round2(x float64) float64 { return math.Round(x*100) / 100 }
