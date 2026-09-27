package voice

import (
	"encoding/binary"
	"math"
	"testing"
)

// tone is n samples of a sine at hz with the given peak (0–1), as float64.
func tone(hz, peak float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = peak * math.Sin(2*math.Pi*hz*float64(i)/playbackRate)
	}
	return out
}

// The band pass keeps the voice and drops what is under and over it.
func TestFilterSpeechKeepsTheVoiceBandOnly(t *testing.T) {
	const n = playbackRate // one second, so the filters have settled
	reference := tone(1000, 0.5, n)
	filterSpeech(reference, playbackRate)
	ref := rmsDB(reference[n/2:])
	if math.Abs(ref-rmsDB(tone(1000, 0.5, n)[n/2:])) > 1 {
		t.Errorf("1 kHz moved by %.1f dB; the voice band must pass untouched", ref-rmsDB(tone(1000, 0.5, n)[n/2:]))
	}
	for _, c := range []struct {
		hz   float64
		what string
	}{{30, "rumble"}, {11000, "hiss"}} {
		s := tone(c.hz, 0.5, n)
		filterSpeech(s, playbackRate)
		if drop := ref - rmsDB(s[n/2:]); drop < 12 {
			t.Errorf("%s at %.0f Hz was only cut by %.1f dB", c.what, c.hz, drop)
		}
	}
}

// Every sentence lands at one loudness: quiet comes up, loud comes down, and
// nothing goes past the ceiling.
func TestLevelSpeechLandsEverySentenceAtOneLoudness(t *testing.T) {
	quiet := tone(440, 0.04, playbackRate/4) // about -31 dBFS RMS
	levelSpeech(quiet)
	if got := rmsDB(quiet); math.Abs(got-clarityTargetRMS) > 1.5 {
		t.Errorf("a quiet sentence came out at %.1f dBFS, want %.0f", got, clarityTargetRMS)
	}
	loud := tone(440, 1.0, playbackRate/4)
	levelSpeech(loud)
	for i, x := range loud {
		if math.Abs(x) >= clarityCeiling {
			t.Fatalf("sample %d at %.3f reached the ceiling", i, x)
		}
	}
	if got := rmsDB(loud); got > -6.5 {
		t.Errorf("a loud sentence was only brought to %.1f dBFS", got)
	}
	whisper := tone(440, 0.0005, playbackRate/4) // about -69 dBFS: the noise floor, not a voice
	before := rmsDB(whisper)
	levelSpeech(whisper)
	if got := rmsDB(whisper); math.Abs(got-before) > 0.01 {
		t.Errorf("the noise floor was raised from %.1f to %.1f dBFS", before, got)
	}
	silence := make([]float64, 100)
	levelSpeech(silence)
	for _, x := range silence {
		if x != 0 {
			t.Fatal("silence did not stay silent")
		}
	}
}

// A clip too short to hold a sample pair is left alone, and a real one comes
// back the same length with its content changed.
func TestClearSpeechWorksOnBytes(t *testing.T) {
	clearSpeech(nil)
	clearSpeech([]byte{1, 2})
	pcm := make([]byte, 2*playbackRate/10)
	for i := 0; i < len(pcm)/2; i++ {
		v := int16(1000 * math.Sin(2*math.Pi*300*float64(i)/playbackRate))
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(v))
	}
	before := rmsDB(toFloat(pcm))
	clearSpeech(pcm)
	if got := rmsDB(toFloat(pcm)); got <= before+6 {
		t.Errorf("a quiet 300 Hz voice went from %.1f to %.1f dBFS; want it brought up", before, got)
	}
}

// The soft limiter is transparent under its knee and never reaches the
// ceiling above it, in both polarities.
func TestSoftLimitRoundsOffRatherThanClips(t *testing.T) {
	if got := softLimit(0.5, clarityCeiling); got != 0.5 {
		t.Errorf("under the knee: %.3f", got)
	}
	for _, x := range []float64{0.9, 1.5, 10, -0.9, -1.5, -10} {
		got := softLimit(x, clarityCeiling)
		if math.Abs(got) > clarityCeiling || math.Signbit(got) != math.Signbit(x) {
			t.Errorf("softLimit(%.1f) = %.3f", x, got)
		}
	}
	if softLimit(1.5, clarityCeiling) <= softLimit(0.9, clarityCeiling) {
		t.Error("the limiter is not monotonic")
	}
}
