package voice

import (
	"encoding/binary"
	"math"
)

// The synthesized voice is cleaned in Go before it reaches the speakers, so
// it is the same voice on every platform and under every mixer: the sound
// server's own effects differ per machine and the user's mixer state is not
// something a spoken answer should depend on. Three passes, in this order,
// on s16le mono at playbackRate:
//
//   - a high-pass at clarityHighPassHz removes rumble and DC a synthesizer
//     leaves under the voice, which is what muddies it on small speakers;
//   - a low-pass at clarityLowPassHz removes the hiss above the speech band,
//     which is where a neural vocoder's artifacts live and where a
//     compressed music stream under the voice fights it hardest;
//   - the level is brought to clarityTargetRMS and peaks are held under
//     clarityCeiling, so every sentence lands at the same loudness whatever
//     the voice model put out, and a loud one does not clip.
//
// The music under it is already ducked to a quarter; this is the other half
// of the same promise, that the voice is clear over whatever else is on.
const (
	clarityHighPassHz = 80.0
	clarityLowPassHz  = 7500.0
	// clarityTargetRMS is the loudness a sentence is brought to, in dBFS.
	// Speech at -18 dBFS RMS is loud without being harsh and leaves 18 dB
	// for its peaks, which is about what voiced consonants need.
	clarityTargetRMS = -18.0
	// clarityMaxBoost bounds how far a quiet clip is raised, so a clip that
	// is mostly silence with one word in it is not raised into noise.
	clarityMaxBoost = 12.0
	// clarityMaxCut bounds how far a loud clip is lowered.
	clarityMaxCut = -6.0
	// clarityGateRMS is the level under which a clip is left alone: there
	// is no voice in it to bring up, only the noise floor.
	clarityGateRMS = -50.0
	// clarityCeiling is the peak nothing is allowed past, as a fraction of
	// full scale: a little headroom under the sound server's own mixing.
	clarityCeiling = 0.95
)

// clearSpeech applies the three passes in place. It is a no-op on an empty
// or odd-length clip.
func clearSpeech(pcm []byte) {
	if len(pcm) < 4 {
		return
	}
	samples := toFloat(pcm)
	filterSpeech(samples, playbackRate)
	levelSpeech(samples)
	fromFloat(samples, pcm)
}

// filterSpeech is the band pass: a second-order high-pass and a fourth-order
// (two cascaded second-order) low-pass, Butterworth both.
func filterSpeech(samples []float64, rate float64) {
	hp := highPass(clarityHighPassHz, rate)
	lp1 := lowPass(clarityLowPassHz, rate)
	lp2 := lowPass(clarityLowPassHz, rate)
	for i, x := range samples {
		samples[i] = lp2.next(lp1.next(hp.next(x)))
	}
}

// levelSpeech brings the clip's RMS toward the target within the boost and
// cut bounds, then holds every peak under the ceiling with a soft knee, so
// the loudest syllable rounds off rather than clips.
func levelSpeech(samples []float64) {
	rms := rmsDB(samples)
	if rms < clarityGateRMS {
		return
	}
	gainDB := math.Max(clarityMaxCut, math.Min(clarityMaxBoost, clarityTargetRMS-rms))
	gain := math.Pow(10, gainDB/20)
	for i, x := range samples {
		samples[i] = softLimit(x*gain, clarityCeiling)
	}
}

// softLimit passes signal under the knee untouched and compresses what is
// above it toward the ceiling asymptotically, so nothing ever reaches it.
func softLimit(x, ceiling float64) float64 {
	knee := ceiling * 0.8
	a := math.Abs(x)
	if a <= knee {
		return x
	}
	over := (a - knee) / (ceiling - knee)
	limited := knee + (ceiling-knee)*math.Tanh(over)
	if x < 0 {
		return -limited
	}
	return limited
}

func rmsDB(samples []float64) float64 {
	if len(samples) == 0 {
		return math.Inf(-1)
	}
	var sum float64
	for _, x := range samples {
		sum += x * x
	}
	rms := math.Sqrt(sum / float64(len(samples)))
	if rms == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}

// biquad is one second-order IIR section in direct form I.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func (f *biquad) next(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

// lowPass and highPass are the Audio EQ Cookbook Butterworth sections
// (Q = 1/sqrt 2).
func lowPass(cutoff, rate float64) *biquad {
	w := 2 * math.Pi * cutoff / rate
	alpha := math.Sin(w) / (2 * math.Sqrt2)
	cw := math.Cos(w)
	a0 := 1 + alpha
	return &biquad{
		b0: (1 - cw) / 2 / a0, b1: (1 - cw) / a0, b2: (1 - cw) / 2 / a0,
		a1: -2 * cw / a0, a2: (1 - alpha) / a0,
	}
}

func highPass(cutoff, rate float64) *biquad {
	w := 2 * math.Pi * cutoff / rate
	alpha := math.Sin(w) / (2 * math.Sqrt2)
	cw := math.Cos(w)
	a0 := 1 + alpha
	return &biquad{
		b0: (1 + cw) / 2 / a0, b1: -(1 + cw) / a0, b2: (1 + cw) / 2 / a0,
		a1: -2 * cw / a0, a2: (1 - alpha) / a0,
	}
}

func toFloat(pcm []byte) []float64 {
	n := len(pcm) / 2
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	return out
}

func fromFloat(samples []float64, pcm []byte) {
	for i, x := range samples {
		v := math.Round(x * 32767)
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(v)))
	}
}
