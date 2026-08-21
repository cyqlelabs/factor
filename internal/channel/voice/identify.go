package voice

import (
	"context"
	"log/slog"
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
	// speakerStickyWindow is how long a short or barged utterance inherits
	// the last identified voice: a "yes" mid-conversation belongs to whoever
	// was just talking, and half a second of audio cannot say who that is.
	speakerStickyWindow = 30 * time.Second

	// speakerMinBytes is the least speech worth embedding — below about a
	// second the vectors are noise, and naming the wrong person costs more
	// than naming nobody. Judged over the utterance minus its silence tail:
	// every segment carries silence_ms of closing quiet, which says nothing
	// about anyone's voice.
	speakerMinBytes = captureRate * 2 // one second of 16 kHz s16le
)

// speakerIdentity is what the channel decided about who spoke. A blank name
// is a voice it will not commit to; primary is the machine's owner, who keeps
// the channel's original session.
type speakerIdentity struct {
	name    string
	primary bool
}

// identifySpeaker names the voice behind one accepted utterance. Barged and
// too-short utterances are never embedded — the first carries the speakers'
// own sound mixed in, the second not enough voice to be anyone reliably —
// they inherit the conversation's current speaker instead.
func (v *Voice) identifySpeaker(ctx context.Context, utterance capturedUtterance) speakerIdentity {
	if v.speakers == nil {
		return speakerIdentity{}
	}
	silenceTail := v.cfg.SilenceMs * captureRate * 2 / 1000
	if utterance.barged || len(utterance.pcm)-silenceTail < speakerMinBytes {
		return v.stickySpeaker()
	}
	embedding, err := v.speechClient().embed(ctx, utterance.pcm)
	if err != nil || len(embedding) == 0 {
		// One warning per outage: with the managed server down or serving
		// without a speaker model, every utterance would repeat it.
		if err != nil && ctx.Err() == nil && !v.embedFailing.Swap(true) {
			slog.Warn("speaker identification failed; not naming speakers until it recovers",
				"error", v.redact(err))
		}
		return v.stickySpeaker()
	}
	v.embedFailing.Store(false)
	name, similarity := v.speakers.match(embedding)
	if name != "" && similarity >= v.cfg.SpeakerThreshold {
		v.speakers.learn(name, embedding)
		return v.rememberSpeaker(name)
	}
	if v.cfg.UnknownSpeaker == unknownEnroll {
		enrolled := v.speakers.enroll(embedding)
		slog.Info("a new voice enrolled", "speaker", enrolled)
		return v.rememberSpeaker(enrolled)
	}
	return v.rememberSpeaker("")
}

// stickySpeaker is the conversation's current voice, while it is current.
func (v *Voice) stickySpeaker() speakerIdentity {
	v.mu.Lock()
	name, at := v.lastSpeaker, v.lastSpeakerAt
	v.mu.Unlock()
	if name == "" || time.Since(at) > speakerStickyWindow {
		return speakerIdentity{}
	}
	return v.identity(name)
}

// rememberSpeaker records who is talking now — including nobody, so an
// unknown voice does not ride the previous speaker's identity.
func (v *Voice) rememberSpeaker(name string) speakerIdentity {
	v.mu.Lock()
	v.lastSpeaker, v.lastSpeakerAt = name, time.Now()
	v.mu.Unlock()
	return v.identity(name)
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

// session is where this identity's conversation lives, and content is what
// the agent reads: a named guest speaks under their own key and their words
// arrive marked, while the owner and the unnamed keep the channel's original
// session, exactly as before identification existed.
func (who speakerIdentity) session() string {
	if who.name == "" || who.primary {
		return sessionKey
	}
	return sessionKey + ":" + speakerSlug(who.name)
}

func (who speakerIdentity) content(text string) string {
	if who.name == "" || who.primary {
		return text
	}
	return "[" + who.name + "] " + text
}
