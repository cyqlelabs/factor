package voice

import (
	"context"
	"encoding/binary"
	"math"
	"time"
)

// The chime answers, where you are standing, the question the log answers
// hours later: Factor heard that, and did not take it. The gate turns a
// sentence away whenever it was not addressed here — no wake word, outside
// the follow-up window — and until now the only evidence was a line in a file
// nobody is reading at the time.
//
// It is a tone rather than a word on purpose. A spoken line would be the
// agent answering an utterance it has just decided was not for it, and would
// come back through the microphone as its own voice.

const (
	// chimeCooldown is the least time between two chimes. Every sentence of
	// a conversation held in front of the machine is turned away, and a tone
	// per sentence is a metronome rather than a courtesy: what it has to say
	// is "heard, not taken", which needs saying once per stretch of talk.
	chimeCooldown = 15 * time.Second

	// The two notes, a descending fifth. Descending because nothing is being
	// asked of anybody — a rising figure is a prompt, and a prompt is exactly
	// what this is not.
	chimeHighHz = 880.00 // A5
	chimeLowHz  = 587.33 // D5

	// chimeSpacing is how long after the first note the second one strikes.
	// They ring together, which is what makes it a chime and not two beeps.
	chimeSpacing = 170 * time.Millisecond
	// chimeRing is how long a note lasts and chimeDecay how fast it fades:
	// the exponential tail a struck bar has, still audible at the end of the
	// ring but well under a tenth of where it started.
	chimeRing  = 700 * time.Millisecond
	chimeDecay = 260 * time.Millisecond
	// chimeAttack rounds the leading edge off. A sine that begins at full
	// amplitude clicks, and a click is the opposite of peaceful.
	chimeAttack = 18 * time.Millisecond

	// chimePeak is the loudest one note gets, as a fraction of full scale. It
	// sits far under speech: this is a courtesy in the background, and one
	// that startles is worse than none at all.
	chimePeak = 0.16
)

// chimePCM renders the chime as the 16-bit little-endian mono PCM the player
// feeds the speakers, at the same volume the spoken voice is turned down to.
func chimePCM(volume int) []byte {
	notes := []struct {
		hz float64
		at time.Duration
	}{
		{chimeHighHz, 0},
		{chimeLowHz, chimeSpacing},
	}
	samples := int((chimeSpacing + chimeRing).Seconds() * playbackRate)
	mix := make([]float64, samples)
	for _, note := range notes {
		start := int(note.at.Seconds() * playbackRate)
		for i := start; i < samples; i++ {
			t := float64(i-start) / playbackRate
			mix[i] += chimeEnvelope(t) * chimeVoice(note.hz, t) * chimePeak
		}
	}
	pcm := make([]byte, 2*samples)
	for i, level := range mix {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(level*math.MaxInt16)))
	}
	scalePCM(pcm, volume)
	return pcm
}

// chimeVoice is one note at time t seconds from its strike: a sine under a
// quiet octave and twelfth, which is what keeps it from sounding like a test
// tone. The weights sum to one, so two notes ringing at once still leave the
// mix a long way inside full scale.
func chimeVoice(hz, t float64) float64 {
	const fundamental, octave, twelfth = 0.62, 0.26, 0.12
	return fundamental*math.Sin(2*math.Pi*hz*t) +
		octave*math.Sin(4*math.Pi*hz*t) +
		twelfth*math.Sin(6*math.Pi*hz*t)
}

// chimeEnvelope shapes one note over time: a raised-cosine edge onto the
// note, then an exponential fade.
func chimeEnvelope(t float64) float64 {
	gain := math.Exp(-t / chimeDecay.Seconds())
	if attack := chimeAttack.Seconds(); t < attack {
		gain *= (1 - math.Cos(math.Pi*t/attack)) / 2
	}
	return gain
}

// chime sounds the tone for a sentence the gate turned away, so "it heard me
// and did nothing" is answered where it is asked rather than only in the log.
// It holds off for chimeCooldown: the gate turns away every sentence of a
// conversation held in front of the machine.
func (v *Voice) chime(ctx context.Context) {
	if !v.cfg.ignoredChime() {
		return
	}
	v.mu.Lock()
	due := time.Since(v.lastChime) >= chimeCooldown
	v.mu.Unlock()
	if !due || !v.player.chime(ctx, chimePCM(v.cfg.OutputVolume)) {
		return
	}
	v.mu.Lock()
	v.lastChime = time.Now()
	v.mu.Unlock()
}
