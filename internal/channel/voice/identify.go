package voice

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Speaker identification: which of the household's voices one utterance is.
// The managed speech server splits the recording into one stretch per person
// and computes an embedding of each; the profile store names them. A known
// voice that is not the machine's owner holds its own conversation — its own
// session key — so each person's thread of dialogue stays theirs. (Session
// history separates; ambient memory is still one shared graph, so a fact one
// person tells Factor can surface in another's recall.)
//
// The split is not a refinement, it is the difference between working and
// not. The segmenter closes an utterance on silence_ms of quiet, and two
// people talking to each other leave far shorter gaps than that, so their
// whole exchange arrives as one recording. Read as one voice it is a blend of
// both, which lands nearest whichever profile it happens to be closest to and
// hands both people that one name — while reporting a room with one person in
// it, which is the reading confidentiality can least afford to get wrong.

const (
	// speakerStickyWindow is how long an utterance that could not be read
	// inherits the last identified voice: a "yes" mid-conversation belongs to
	// whoever was just talking, and half a second of audio cannot say who
	// that is.
	speakerStickyWindow = 30 * time.Second

	// The three length bars, in seconds of actual speech — gaps and overlaps
	// already removed by the server, so these mean what they say.
	//
	// They rise with what is at stake. Naming a voice risks one misattributed
	// sentence. Teaching a profile moves the centroid every later match is
	// judged against, so a bad one is paid for repeatedly. Creating a profile
	// is worst of all: a spurious one never goes away on its own, it competes
	// for every future match, and a store full of ghosts is how "it stopped
	// recognizing me" begins.
	//
	// The numbers are measured against real utterances off a real microphone,
	// not guessed: the same person's shortest usable stretch, three quarters
	// of a second, still scores 0.49 against their other utterances while
	// every impostor stays under 0.10. Naming survives short speech; the bars
	// above it exist because a profile built from a scrap is wrong forever,
	// not because a scrap cannot be matched.
	speakerMatchMinSeconds  = 1.0
	speakerLearnMinSeconds  = 2.0
	speakerEnrollMinSeconds = 3.0

	// speakerLearnMargin is how far above the threshold a match has to sit
	// before it is folded into the profile. Matching and learning are
	// different bets: a borderline match costs one misattributed sentence,
	// while borderline audio folded into a centroid moves it — toward the
	// room, toward whoever else was talking — and every later match is judged
	// against the moved one. That is how profiles drift into each other, and
	// it is what "it stopped recognizing me" looks like from the inside.
	speakerLearnMargin = 0.15

	// speakerRunnerUpMargin is how far the closest profile has to sit ahead
	// of the next one before the read is treated as evidence of who spoke.
	//
	// A threshold alone cannot carry this. Measured on this machine the two
	// score distributions overlap — the same person has matched their own
	// profile at 0.36, and a different person has come within 0.45 of one —
	// so "above the threshold" describes a great many readings that are
	// really a coin flip between two profiles. The distance back to the
	// runner-up is what separates them: a genuine match leaves the wrong
	// profile far behind, while a blend of two voices, or a bad second of
	// audio, lands between them and lands close to both.
	speakerRunnerUpMargin = 0.10

	// A flat threshold is the wrong shape, because how much a similarity is
	// worth depends on how much voice it was computed over. Across 221 real
	// matches on this machine the median rises with the reading and the floor
	// rises with it too:
	//
	//	under 1.5s   median 0.61, and 18% of true matches under 0.50
	//	1.5s to 3s   median 0.70, and 7% under 0.50
	//	3s to 6s     median 0.77, and 1% under 0.50
	//	over 6s      median 0.83, and none under 0.69
	//
	// Meanwhile a different person has been measured within 0.45 of a
	// profile. So the configured threshold is only safe for a reading long
	// enough to have earned it: below speakerConfidentSeconds the bar rises
	// toward speakerShortMargin above it, reaching the top at the shortest
	// reading that gets read at all. It is the same idea as the length bars
	// above — what is at stake decides the bar — applied to the score rather
	// than to the decision.
	speakerConfidentSeconds = 3.0
	speakerShortMargin      = 0.15
)

// How one identity was arrived at. It rides the decision purely so the log
// can say it: "it answered as the wrong person" is only diagnosable if the
// line names which branch ran and how close the vectors were.
const (
	viaMatch       = "match"       // a profile above the threshold
	viaEnrolled    = "enrolled"    // nobody matched, so a profile was created
	viaUnknown     = "unknown"     // nobody matched, and no profile was created
	viaOverlap     = "overlap"     // recorded with the agent's voice in it: not read
	viaShort       = "short"       // too little speech to identify
	viaUnsure      = "unsure"      // over the threshold, under the bar this reading's length demands
	viaAmbiguous   = "ambiguous"   // two profiles too close together to choose between
	viaUnavailable = "unavailable" // the recording could not be read at all
)

// speakerIdentity is what the channel decided about one voice. A blank name
// is a voice it will not commit to; primary is the machine's owner, who keeps
// the channel's original session.
type speakerIdentity struct {
	name    string
	primary bool
	// runnerUp is how close the second-closest profile came. It rides along
	// only for the ambiguous branch, which is the one decision the winning
	// score alone cannot explain.
	runnerUp float64
	// bar is the similarity this reading had to clear, which is not the
	// configured threshold whenever the reading was brief.
	bar float64

	// via and similarity are the decision's own account of itself, for the
	// log: which branch named this voice, and how close the closest profile
	// was — the number speaker_threshold is tuned against. seconds is how
	// much speech it was judged on, which is the other half of why a
	// similarity came out where it did.
	via        string
	similarity float64
	seconds    float64
	// inherited marks a name taken from the conversation rather than from
	// this utterance's own audio.
	inherited bool
}

// heardVoices is what one utterance's audio said about the people in it:
// which of them the turn belongs to, and every voice the recording held.
// They are different questions — the first decides whose conversation this
// is, the second decides who can hear the answer — and reading one recording
// as one voice is what used to conflate them.
type heardVoices struct {
	speaker speakerIdentity
	present []speakerIdentity
}

// identifySpeaker names the voices behind one accepted utterance, and lets
// what it hears teach the profiles.
func (v *Voice) identifySpeaker(ctx context.Context, utterance capturedUtterance) heardVoices {
	return v.readVoices(ctx, utterance, true)
}

// presenceOf reads the voices behind an utterance the activation gate turned
// away — speech in the room that was not addressed to Factor. It answers only
// the room's question, and it answers it read-only: it must not enroll a
// profile, must not fold the audio into one, and must not move the sticky
// speaker. Ambient conversation is the wrong teacher for all three — a
// television would mint profiles, and a sentence spoken across the room would
// steal the name a following "yes" inherits.
func (v *Voice) presenceOf(ctx context.Context, utterance capturedUtterance) heardVoices {
	return v.readVoices(ctx, utterance, false)
}

// readVoices is the shared body of both. teach is what separates them: with
// it off nothing about the store or the conversation moves, which is the
// whole licence ambient speech gets.
func (v *Voice) readVoices(ctx context.Context, utterance capturedUtterance, teach bool) heardVoices {
	if v.speakers == nil {
		return heardVoices{}
	}
	// Audio the agent's own voice is in holds two speakers by construction,
	// and one of them is a synthesizer. Reading it would name the wrong
	// person and teaching from it would move a profile toward a machine.
	if utterance.overlapped {
		return v.unread(viaOverlap, teach)
	}
	readings, model, err := v.speechClient().voices(ctx, utterance.pcm)
	if err != nil {
		// One warning per outage: with the managed server down or serving
		// without a speaker model, every utterance would repeat it.
		if ctx.Err() == nil && !v.embedFailing.Swap(true) {
			slog.Warn("speaker identification failed; not naming speakers until it recovers",
				"error", v.redact(err))
		}
		return v.unread(viaUnavailable, teach)
	}
	if v.embedFailing.Swap(false) {
		slog.Info("speaker identification recovered")
	}
	v.speakers.useModel(model)
	if len(readings) == 0 {
		return v.unread(viaShort, teach)
	}
	// Nobody else may have been in the recording for it to move the store at
	// all — neither to teach a profile nor to mint one. A diarized stretch is
	// clean by construction, but "clean" rests on the split having been
	// right, and a profile is the one thing here that outlives the mistake.
	//
	// Minting used to be exempt from this, which had it backwards: teaching
	// moves a centroid that later matches are judged against, but a spurious
	// profile never goes away, competes for every future match, and is how a
	// household of three ends up with fifteen voices. The more expensive
	// mistake cannot have the looser bar. Nothing is lost by waiting —
	// somebody who lives here says something on their own soon enough.
	alone := len(readings) == 1
	present := make([]speakerIdentity, 0, len(readings))
	for _, reading := range readings {
		present = append(present, v.nameVoice(ctx, reading, teach && alone))
	}
	if !teach {
		// The ambient path names the voices for the room and for the log, but
		// commits to nothing: no sticky speaker, no session, no profile moved.
		return heardVoices{speaker: present[0], present: present}
	}
	// The turn belongs to whoever opened the recording. The activation gate
	// only let this through because the wake word was at its front, or
	// because it answered inside the follow-up window — in both cases the
	// person who started talking is the one talking to Factor, and anyone who
	// joined partway through is the room.
	speaker := present[0]
	if speaker.name == "" && speaker.unreadable() {
		// Too little voice to name anybody, too little daylight between two
		// profiles to choose, or a score too weak for how brief the reading
		// was — but somebody is plainly mid-conversation: a "yes" belongs to
		// whoever was just talking. Inheriting is right for all three where
		// clearing is right for viaUnknown — that one is a voice long enough
		// to read that matched nobody, which is evidence of a different
		// person rather than of an unreadable moment.
		inherited := v.stickySpeaker(speaker.via)
		// Which of the three reasons it was, and the numbers behind it, are
		// why this branch ran — so they travel on even though the name did
		// not come from them. Reporting all of them as "short" would hide an
		// ambiguity behind a length, and each is tuned by its own knob.
		inherited.similarity = speaker.similarity
		inherited.runnerUp = speaker.runnerUp
		inherited.bar = speaker.bar
		inherited.seconds = speaker.seconds
		return heardVoices{speaker: inherited, present: present}
	}
	return heardVoices{speaker: v.rememberSpeaker(speaker), present: present}
}

// nameVoice matches one voice against the profiles. commit allows it to move
// the store — to fold the reading into the profile it matched, or to create
// one for a voice that matched nothing.
func (v *Voice) nameVoice(ctx context.Context, reading voiceReading, commit bool) speakerIdentity {
	if reading.Seconds < speakerMatchMinSeconds || len(reading.Embedding) == 0 {
		return speakerIdentity{via: viaShort, seconds: reading.Seconds}
	}
	m := v.speakers.match(reading.Embedding)
	name, similarity := m.name, m.best
	// Every profile's score, for tuning speaker_threshold against real voices
	// — the one question the decision alone cannot answer is how close the
	// runners-up were. Built only when the log would print it.
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.Debug("speaker scores", "scores", v.speakers.scores(reading.Embedding),
			"seconds", round2(reading.Seconds), "threshold", v.cfg.SpeakerThreshold)
	}
	if name != "" && similarity >= v.cfg.SpeakerThreshold {
		if bar := v.matchBar(reading.Seconds); similarity < bar {
			// Over the configured threshold on a reading too brief for that
			// to mean much. Treated as an unreadable moment rather than as a
			// stranger: a couple of seconds is the ordinary shape of a reply
			// mid-conversation, and answering it as nobody would put the
			// person who never stopped talking into a different session.
			return speakerIdentity{via: viaUnsure, similarity: similarity,
				bar: bar, seconds: reading.Seconds}
		}
		if similarity-m.runnerUp < speakerRunnerUpMargin {
			// Close to two profiles at once. Naming the nearer one would be
			// a coin flip, and folding this reading into it would pull the
			// two closer still — which is how two people's profiles collapse
			// into one and neither is ever recognized again.
			return speakerIdentity{via: viaAmbiguous, similarity: similarity,
				runnerUp: m.runnerUp, seconds: reading.Seconds}
		}
		if commit && similarity >= v.cfg.SpeakerThreshold+speakerLearnMargin &&
			reading.Seconds >= speakerLearnMinSeconds {
			v.speakers.learn(name, reading.Embedding)
		}
		return v.identity(name, viaMatch, similarity, reading.Seconds)
	}
	if commit && v.cfg.UnknownSpeaker == unknownEnroll && reading.Seconds >= speakerEnrollMinSeconds {
		enrolled := v.speakers.enroll(reading.Embedding)
		slog.Info("a new voice enrolled", "speaker", enrolled, "closest", name,
			"similarity", round2(similarity), "threshold", v.cfg.SpeakerThreshold,
			"seconds", round2(reading.Seconds))
		return v.identity(enrolled, viaEnrolled, similarity, reading.Seconds)
	}
	return speakerIdentity{via: viaUnknown, similarity: similarity, seconds: reading.Seconds}
}

// matchBar is the similarity this reading has to clear to name somebody. It
// is the configured threshold for a reading long enough to be judged on it,
// rising toward speakerShortMargin above that as the reading shortens. Only
// ever called for a reading past speakerMatchMinSeconds, which is where the
// slope tops out.
func (v *Voice) matchBar(seconds float64) float64 {
	if seconds >= speakerConfidentSeconds {
		return v.cfg.SpeakerThreshold
	}
	brief := (speakerConfidentSeconds - seconds) /
		(speakerConfidentSeconds - speakerMatchMinSeconds)
	return v.cfg.SpeakerThreshold + brief*speakerShortMargin
}

// unread is the answer for an utterance whose voices could not be read at
// all. On the addressed path the conversation's current speaker stands in, so
// a "yes" over the agent's own voice still belongs to whoever just spoke; on
// the ambient path nothing does, because a name invented there would be
// evidence of a person nobody heard.
func (v *Voice) unread(via string, teach bool) heardVoices {
	if !teach {
		return heardVoices{speaker: speakerIdentity{via: via}}
	}
	who := v.stickySpeaker(via)
	return heardVoices{speaker: who, present: []speakerIdentity{who}}
}

// speakerProfilesExist reports whether any voice is enrolled, which is what
// makes an unmatched voice mean "not the owner" rather than "the first voice".
func (v *Voice) speakerProfilesExist() bool {
	return v.speakers != nil && v.speakers.hasProfiles()
}

// stickySpeaker is the conversation's current voice, while it is current.
//
// Inheriting also renews the window. The alternative measures the window from
// the last utterance long enough to identify, which means a run of short
// replies — the ordinary shape of a conversation once it is under way —
// silently runs out of time and the person who never stopped talking becomes
// nobody mid-sentence.
func (v *Voice) stickySpeaker(via string) speakerIdentity {
	v.mu.Lock()
	name, at := v.lastSpeaker, v.lastSpeakerAt
	if name != "" && time.Since(at) <= speakerStickyWindow {
		v.lastSpeakerAt = time.Now()
	}
	v.mu.Unlock()
	if name == "" || time.Since(at) > speakerStickyWindow {
		return speakerIdentity{via: via}
	}
	who := v.identity(name, via, 0, 0)
	who.inherited = true
	return who
}

// rememberSpeaker records who is talking now — including nobody, so an
// unknown voice does not ride the previous speaker's identity.
func (v *Voice) rememberSpeaker(who speakerIdentity) speakerIdentity {
	v.mu.Lock()
	v.lastSpeaker, v.lastSpeakerAt = who.name, time.Now()
	v.mu.Unlock()
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

func (v *Voice) identity(name, via string, similarity, seconds float64) speakerIdentity {
	if name == "" {
		return speakerIdentity{via: via, similarity: similarity, seconds: seconds}
	}
	return speakerIdentity{name: name, primary: v.speakers.isPrimary(name),
		via: via, similarity: similarity, seconds: seconds}
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

// unreadable reports whether this is a moment the reader could not commit to,
// as against a voice it read and did not recognize. The three of them behave
// alike: none names anybody, none moves the store, and each inherits whoever
// was already talking rather than declaring a stranger.
func (who speakerIdentity) unreadable() bool {
	switch who.via {
	case viaShort, viaAmbiguous, viaUnsure:
		return true
	}
	return false
}

func (who speakerIdentity) attributed() string {
	if who.primary {
		return ""
	}
	return who.name
}

// logFields is how a decision explains itself on the "voice heard" line: who
// it settled on, which branch decided that, how close the closest profile was
// and on how much speech, and the session the turn will run in — the answers
// behind "why did it answer as the wrong person", and behind "why did it
// answer as nobody" when a threshold or a length bar is the reason.
func (who speakerIdentity) logFields(session string, addressed bool) []any {
	name := who.name
	if name == "" {
		name = "unnamed"
	}
	fields := []any{"speaker", name, "via", who.via}
	switch who.via {
	case viaMatch, viaEnrolled, viaUnknown:
		fields = append(fields, "similarity", round2(who.similarity), "seconds", round2(who.seconds))
	case viaAmbiguous:
		fields = append(fields, "similarity", round2(who.similarity),
			"runner_up", round2(who.runnerUp), "seconds", round2(who.seconds))
	case viaUnsure:
		fields = append(fields, "similarity", round2(who.similarity),
			"bar", round2(who.bar), "seconds", round2(who.seconds))
	case viaShort:
		fields = append(fields, "seconds", round2(who.seconds))
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
