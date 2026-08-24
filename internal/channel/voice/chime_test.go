package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// peakLevel is the loudest sample in a stretch of s16le PCM, as a fraction of
// full scale.
func peakLevel(pcm []byte) float64 {
	var peak float64
	for i := 0; i+1 < len(pcm); i += 2 {
		if level := math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[i:])))); level > peak {
			peak = level
		}
	}
	return peak / math.MaxInt16
}

// toneEnergy measures how much of one frequency a stretch of PCM holds, by
// Goertzel. The chime is tested by reading its notes back out of the rendered
// samples rather than off the constants that rendered them, so an envelope or
// a mix that is wrong cannot agree with the test.
func toneEnergy(pcm []byte, hz float64) float64 {
	samples := len(pcm) / 2
	if samples == 0 {
		return 0
	}
	coeff := 2 * math.Cos(2*math.Pi*hz/playbackRate)
	var prev, prev2 float64
	for i := range samples {
		level := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / math.MaxInt16
		prev, prev2 = level+coeff*prev-prev2, prev
	}
	return math.Sqrt(math.Abs(prev*prev+prev2*prev2-coeff*prev*prev2)) / float64(samples)
}

// ms is that many milliseconds of playback, in bytes.
func ms(d time.Duration) int {
	return 2 * int(d.Seconds()*playbackRate)
}

func TestChimeIsASoftPairOfNotes(t *testing.T) {
	pcm := chimePCM(100)
	if want := ms(chimeSpacing + chimeRing); len(pcm) != want {
		t.Errorf("chime is %d bytes, want %d", len(pcm), want)
	}

	// Audible, and a long way under the level a reply is spoken at: a
	// courtesy that startles is worse than none.
	peak := peakLevel(pcm)
	if peak < 0.05 || peak > 2*chimePeak {
		t.Errorf("chime peaks at %.3f of full scale, want a soft tone", peak)
	}

	// Both notes are in the audio, and the space between them is empty: a
	// chime, not a buzz.
	high := toneEnergy(pcm, chimeHighHz)
	low := toneEnergy(pcm, chimeLowHz)
	between := toneEnergy(pcm, (chimeHighHz+chimeLowHz)/2)
	if high < 4*between || low < 4*between {
		t.Errorf("notes at %.5f and %.5f against %.5f between them", high, low, between)
	}

	// The low note strikes second, so it is barely there before the spacing
	// has elapsed.
	if early := toneEnergy(pcm[:ms(chimeSpacing*9/10)], chimeLowHz); early > low/4 {
		t.Errorf("the second note sounds at %.5f before its strike, against %.5f overall", early, low)
	}

	// It opens on an edge too soft to click and fades rather than stopping.
	if edge := peakLevel(pcm[:ms(5*time.Millisecond)]); edge > peak/2 {
		t.Errorf("the chime opens at %.3f of a %.3f peak: that is a click", edge, peak)
	}
	if tail := peakLevel(pcm[len(pcm)-ms(50*time.Millisecond):]); tail > peak/5 {
		t.Errorf("the chime still sounds at %.3f when it is cut off", tail)
	}
}

// The chime is turned down with the voice: on a machine whose speakers reach
// the microphone, a tone at full volume would feed back where the reply does
// not.
func TestChimeFollowsTheOutputVolume(t *testing.T) {
	full, half := peakLevel(chimePCM(100)), peakLevel(chimePCM(50))
	if ratio := half / full; ratio < 0.45 || ratio > 0.55 {
		t.Errorf("half volume peaks at %.3f of full, want about a half", ratio)
	}
}

func TestPlayerChimeLeavesAReplyTheSpeakers(t *testing.T) {
	speaker, p := speakerAndPlayer()
	done := p.play(context.Background(), clip(96000)) // 2 s
	waitUntil(t, func() bool { return len(speaker.heard()) > 0 })
	if p.chime(context.Background(), chimePCM(100)) {
		t.Error("the chime talked over a reply")
	}
	p.pause()
	if p.chime(context.Background(), chimePCM(100)) {
		t.Error("the chime took the speakers from a paused reply")
	}
	p.stop()
	<-done
	waitUntil(t, func() bool { return !p.playing() })

	if !p.chime(context.Background(), chimePCM(100)) {
		t.Fatal("the chime never sounded over silence")
	}
	// A tone is sound in the room — the microphone's bar rises for it — but
	// it is not the agent's voice, so there is nothing there to interrupt.
	playing, speaking := p.sound()
	if !playing || speaking {
		t.Errorf("a tone reports playing=%v speaking=%v, want true and false", playing, speaking)
	}
	waitUntil(t, func() bool { _, sp := p.sound(); return !sp && !p.busy() })
}

// The whole path: a sentence the wake-word gate turns away is answered with
// the tone and nothing else — no turn, and not a word synthesised, because a
// spoken line would be the agent replying to an utterance it just decided was
// not for it.
func TestVoiceChimesForASentenceItTurnedAway(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "wake-word" })
	h.start()
	h.setTranscript("nothing for you")
	h.say()

	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })
	if heard := h.speaker.heard(); !bytes.HasPrefix(chimePCM(100), heard) {
		t.Errorf("the speakers played %d bytes that are not the chime", len(heard))
	}
	if texts := h.synthesized(); len(texts) != 0 {
		t.Errorf("synthesized %v, want silence from the voice", texts)
	}
	h.noTurn(time.Second)
}

// Sound that is not speech must not chime: nearly a fifth of what the
// detector opens transcribes to nothing, and a tone per door slam is a fault
// report, not a courtesy.
func TestVoiceStaysSilentForNoise(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "wake-word" })
	h.start()
	for _, transcript := range []string{"", "Thanks for watching!"} {
		h.setTranscript(transcript)
		h.say()
		h.noTurn(time.Second)
		if heard := h.speaker.heard(); len(heard) != 0 {
			t.Errorf("transcript %q sounded %d bytes at the speakers", transcript, len(heard))
		}
	}
}

// The gate turns away every sentence of a conversation held in front of the
// machine, so the chime holds off between them.
func TestVoiceChimeHoldsOffBetweenSentences(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "wake-word" })
	h.start()
	h.setTranscript("nothing for you")
	h.say()
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })
	h.waitQuiet()

	h.say()
	h.noTurn(time.Second)
	if runs := h.speaker.processes(); runs != 1 {
		t.Errorf("%d clips reached the speakers, want the one chime", runs)
	}
}

// The chime is a tone, not a voice, so a sentence spoken across it is not an
// interruption — the wake word is still required of it. Without that the
// courtesy would be a trap: the tone says "not for me", and the next thing
// said in the room would become a turn.
func TestVoiceChimeIsNotSomethingToBargeInOn(t *testing.T) {
	h := newVoiceHarness(t, func(c *Config) { c.Activation = "wake-word" })
	h.start()
	h.setTranscript("nothing for you")
	h.say()
	waitUntil(t, func() bool { return len(h.speaker.heard()) > 0 })

	if playing, speaking := h.v.player.sound(); !playing || speaking {
		t.Fatalf("the tone is not in the air to talk across: playing=%v speaking=%v", playing, speaking)
	}
	// Loud enough to clear the raised bar the speakers put up, which is what
	// makes an utterance a barge.
	h.mic.feed(repeat(toneFrame(16000), 12)...)
	h.mic.feed(repeat(silenceFrame(), silenceEndFrames)...)
	h.noTurn(2 * time.Second)
}

func TestVoiceChimeCanBeTurnedOff(t *testing.T) {
	off := false
	h := newVoiceHarness(t, func(c *Config) {
		c.Activation = "wake-word"
		c.IgnoredChime = &off
	})
	h.start()
	h.setTranscript("nothing for you")
	h.say()
	h.noTurn(2 * time.Second)
	if heard := h.speaker.heard(); len(heard) != 0 {
		t.Errorf("the chime sounded %d bytes with the knob off", len(heard))
	}
}
