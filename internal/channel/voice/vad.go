package voice

import (
	"encoding/binary"
	"math"
)

// Voice activity detection, done the plain way: the RMS energy of each frame
// against an adaptive noise floor. It is not a speech model — it does not need
// to be, because everything it admits still has to survive transcription, and
// the transcriber returning nothing is how a slammed door gets dropped. What
// it must get right is timing: open fast enough to keep the first syllable
// (the pre-roll), close late enough to survive a mid-sentence pause, and hold
// a higher bar while the agent is speaking so the speakers cannot barge in on
// themselves.

const (
	frameMs    = 30
	frameBytes = captureRate * 2 * frameMs / 1000 // 16-bit mono

	// preRollFrames of context are kept before speech opens, so detection
	// latency does not eat the first syllable.
	preRollFrames = 10

	// Frames of consecutive speech that open an utterance: three when idle,
	// more during playback — interrupting the agent should take a word, not
	// a cough (the same reasoning as Patter's 300 ms barge-in threshold).
	startFramesIdle    = 3
	startFramesPlaying = 8

	// defaultSilenceMs is how much silence closes an utterance. Two seconds
	// is deliberately patient: a thinking pause mid-sentence must not hand
	// half a thought to the agent, which then talks over the rest. The cost
	// is a beat of latency before every reply; silence_ms tunes it.
	defaultSilenceMs    = 2000
	maxUtteranceSeconds = 30
	// minUtteranceMs of speech below which the segment is discarded unheard.
	minUtteranceMs = 300

	defaultVADRatio   = 3.0
	defaultBargeRatio = 6.0

	// floorAlpha is the noise-floor EMA weight; minFloor keeps the threshold
	// meaningful on digitally silent inputs.
	floorAlpha = 0.05
	minFloor   = 120.0
)

// segmenter turns a stream of PCM frames into utterances.
type segmenter struct {
	ratio      float64
	bargeRatio float64
	silenceMs  int

	floor        float64
	inSpeech     bool
	run          int // consecutive speech frames while idle
	silence      int // consecutive silence frames while in speech
	speechFrames int // frames of actual speech in the open segment
	buf          []byte
	preroll      [][]byte
}

func newSegmenter(ratio, bargeRatio float64, silenceMs int) *segmenter {
	return &segmenter{ratio: ratio, bargeRatio: bargeRatio, silenceMs: silenceMs}
}

// push consumes one frame. started reports the frame that opened an
// utterance (the caller pauses playback there); ended reports an utterance
// boundary, with utterance nil when the segment was too short to mean
// anything. playing raises the bar and freezes the noise floor, which would
// otherwise learn the agent's own voice as background.
func (s *segmenter) push(frame []byte, playing bool) (started bool, ended bool, utterance []byte) {
	level := rms(frame)
	if s.floor == 0 {
		s.floor = math.Max(level, minFloor)
		return false, false, nil
	}

	threshold := s.floor * s.ratio
	if playing {
		threshold = s.floor * s.bargeRatio
	}
	speech := level >= threshold

	if !s.inSpeech {
		s.preroll = append(s.preroll, append([]byte(nil), frame...))
		if len(s.preroll) > preRollFrames {
			s.preroll = s.preroll[1:]
		}
		if !speech {
			s.run = 0
			if !playing {
				s.floor += floorAlpha * (level - s.floor)
				s.floor = math.Max(s.floor, minFloor)
			}
			return false, false, nil
		}
		s.run++
		need := startFramesIdle
		if playing {
			need = startFramesPlaying
		}
		if s.run < need {
			return false, false, nil
		}
		s.inSpeech, s.silence, s.speechFrames = true, 0, s.run
		s.run = 0
		s.buf = s.buf[:0]
		for _, kept := range s.preroll {
			s.buf = append(s.buf, kept...)
		}
		s.preroll = s.preroll[:0]
		return true, false, nil
	}

	s.buf = append(s.buf, frame...)
	if speech {
		s.silence = 0
		s.speechFrames++
	} else {
		s.silence++
	}
	if s.silence*frameMs >= s.silenceMs || len(s.buf) >= maxUtteranceSeconds*captureRate*2 {
		return false, true, s.take()
	}
	return false, false, nil
}

// take closes the current segment, dropping one too short to hold a word.
func (s *segmenter) take() []byte {
	utterance := s.buf
	spoken := s.speechFrames * frameMs
	s.inSpeech, s.buf, s.silence, s.speechFrames = false, nil, 0, 0
	if spoken < minUtteranceMs {
		return nil
	}
	return utterance
}

// rms is the root-mean-square level of one s16le frame.
func rms(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(frame); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(frame[i:])))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// scalePCM lowers s16le audio to a percentage of full volume, in place. 100
// or more leaves the samples untouched.
func scalePCM(pcm []byte, percent int) {
	if percent >= 100 {
		return
	}
	gain := float64(percent) / 100
	for i := 0; i+1 < len(pcm); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) * gain
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(math.Round(v))))
	}
}

// wavPCM wraps raw s16le mono PCM in the 44-byte RIFF header the
// transcription APIs expect.
func wavPCM(pcm []byte, rate int) []byte {
	out := make([]byte, 44+len(pcm))
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)             // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:], 1)              // PCM
	binary.LittleEndian.PutUint16(out[22:], 1)              // mono
	binary.LittleEndian.PutUint32(out[24:], uint32(rate))   // sample rate
	binary.LittleEndian.PutUint32(out[28:], uint32(rate*2)) // byte rate
	binary.LittleEndian.PutUint16(out[32:], 2)              // block align
	binary.LittleEndian.PutUint16(out[34:], 16)             // bits per sample
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}
