package voice

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// silence and tone build one 30 ms frame each; the segmenter sees the world
// only through RMS levels, so a square wave is as good as speech.
func silenceFrame() []byte { return make([]byte, frameBytes) }

func toneFrame(amplitude int16) []byte {
	frame := make([]byte, frameBytes)
	for i := 0; i+1 < len(frame); i += 2 {
		binary.LittleEndian.PutUint16(frame[i:], uint16(amplitude))
	}
	return frame
}

func feed(seg *segmenter, frames [][]byte, playing bool) (started int, utterances [][]byte) {
	for _, frame := range frames {
		s, _, utterance := seg.push(frame, playing)
		if s {
			started++
		}
		if utterance != nil {
			utterances = append(utterances, utterance)
		}
	}
	return started, utterances
}

func repeat(frame []byte, n int) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		frames[i] = frame
	}
	return frames
}

const silenceEndFrames = defaultSilenceMs/frameMs + 1

func TestSegmenterCutsAnUtteranceOutOfSpeechBetweenSilences(t *testing.T) {
	seg := newSegmenter(defaultVADRatio, defaultBargeRatio, defaultSilenceMs)

	var frames [][]byte
	frames = append(frames, repeat(silenceFrame(), 20)...)
	frames = append(frames, repeat(toneFrame(5000), 30)...) // ~900 ms of speech
	frames = append(frames, repeat(silenceFrame(), silenceEndFrames)...)

	started, utterances := feed(seg, frames, false)
	if started != 1 {
		t.Fatalf("speech opened %d times, want once", started)
	}
	if len(utterances) != 1 {
		t.Fatalf("got %d utterances, want 1", len(utterances))
	}
	// The pre-roll must be included: detection latency cannot eat a syllable.
	if len(utterances[0]) < 30*frameBytes {
		t.Errorf("utterance is %d bytes, shorter than the speech it heard", len(utterances[0]))
	}
	if !bytes.Contains(utterances[0], toneFrame(5000)) {
		t.Error("the utterance does not contain the speech frames")
	}
}

func TestSegmenterDiscardsACough(t *testing.T) {
	seg := newSegmenter(defaultVADRatio, defaultBargeRatio, defaultSilenceMs)

	var frames [][]byte
	frames = append(frames, repeat(silenceFrame(), 20)...)
	frames = append(frames, repeat(toneFrame(5000), 4)...) // 120 ms: opens, but too short
	frames = append(frames, repeat(silenceFrame(), silenceEndFrames)...)

	started, utterances := feed(seg, frames, false)
	if started != 1 {
		t.Fatalf("speech opened %d times, want once", started)
	}
	if len(utterances) != 0 {
		t.Fatalf("a 120 ms burst produced an utterance")
	}

	// The segmenter must be reusable after a discard.
	var again [][]byte
	again = append(again, repeat(toneFrame(5000), 30)...)
	again = append(again, repeat(silenceFrame(), silenceEndFrames)...)
	if _, utterances := feed(seg, again, false); len(utterances) != 1 {
		t.Errorf("got %d utterances after a discard, want 1", len(utterances))
	}
}

func TestSegmenterIgnoresNoiseBelowTheThreshold(t *testing.T) {
	seg := newSegmenter(defaultVADRatio, defaultBargeRatio, defaultSilenceMs)
	// Steady room noise around the floor: never three times above it.
	started, utterances := feed(seg, repeat(toneFrame(200), 100), false)
	if started != 0 || len(utterances) != 0 {
		t.Errorf("background noise opened speech (started=%d, utterances=%d)", started, len(utterances))
	}
}

func TestSegmenterAdaptsItsFloorToTheRoom(t *testing.T) {
	seg := newSegmenter(defaultVADRatio, defaultBargeRatio, defaultSilenceMs)
	// A noisy room: the floor learns 1000, so 2000 is no longer speech...
	feed(seg, repeat(toneFrame(1000), 200), false)
	if started, _ := feed(seg, repeat(toneFrame(2000), 10), false); started != 0 {
		t.Error("a level twice the learned floor opened speech")
	}
	// ...but well above it still is.
	if started, _ := feed(seg, repeat(toneFrame(8000), 10), false); started != 1 {
		t.Error("a level far above the learned floor did not open speech")
	}
}

// During playback the bar is higher and slower: the speakers' own sound must
// not interrupt the reply, and a real interruption should take a word.
func TestSegmenterHoldsAHigherBarWhilePlaying(t *testing.T) {
	seg := newSegmenter(defaultVADRatio, defaultBargeRatio, defaultSilenceMs)
	feed(seg, repeat(silenceFrame(), 20), false) // settle the floor at the minimum

	// Loud enough to open speech when idle (>3× floor), not loud enough to
	// barge in (<6× floor).
	echo := toneFrame(int16(minFloor * 4))
	if started, _ := feed(seg, repeat(echo, 20), true); started != 0 {
		t.Error("playback echo barged in on the reply")
	}

	// A genuinely loud voice does, after more frames than the idle threshold.
	loud := repeat(toneFrame(8000), startFramesPlaying)
	started, _ := feed(seg, loud[:startFramesIdle], true)
	if started != 0 {
		t.Error("a barge-in opened as fast as idle speech; it should take longer")
	}
	if started, _ := feed(seg, loud[startFramesIdle:], true); started != 1 {
		t.Error("a loud voice never barged in")
	}
}

func TestSegmenterFreezesTheFloorWhilePlaying(t *testing.T) {
	seg := newSegmenter(defaultVADRatio, defaultBargeRatio, defaultSilenceMs)
	feed(seg, repeat(silenceFrame(), 20), false)
	floor := seg.floor
	// A minute of the agent's own voice must not teach the floor that this
	// is what the room sounds like.
	feed(seg, repeat(toneFrame(600), 2000), true)
	if seg.floor != floor {
		t.Errorf("the floor drifted from %v to %v during playback", floor, seg.floor)
	}
}

func TestSegmenterForcesAnEndOnAnEndlessUtterance(t *testing.T) {
	seg := newSegmenter(defaultVADRatio, defaultBargeRatio, defaultSilenceMs)
	feed(seg, repeat(silenceFrame(), 20), false)

	// Speech that never stops — a stuck alarm — must still produce segments
	// rather than an unbounded buffer.
	frames := maxUtteranceSeconds*1000/frameMs + 100
	_, utterances := feed(seg, repeat(toneFrame(5000), frames), false)
	if len(utterances) == 0 {
		t.Fatal("an endless utterance never closed")
	}
	if len(utterances[0]) > maxUtteranceSeconds*captureRate*2+frameBytes {
		t.Errorf("utterance grew to %d bytes, past the cap", len(utterances[0]))
	}
}

func TestRMS(t *testing.T) {
	if got := rms(silenceFrame()); got != 0 {
		t.Errorf("rms(silence) = %v", got)
	}
	if got := rms(toneFrame(1000)); got < 999 || got > 1001 {
		t.Errorf("rms(constant 1000) = %v", got)
	}
	if got := rms(nil); got != 0 {
		t.Errorf("rms(nil) = %v", got)
	}
}

func TestWavPCMWrapsAPlayableHeader(t *testing.T) {
	pcm := toneFrame(1000)
	wav := wavPCM(pcm, captureRate)
	if len(wav) != 44+len(pcm) {
		t.Fatalf("wav length = %d", len(wav))
	}
	if string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Error("missing RIFF/WAVE magic")
	}
	if rate := binary.LittleEndian.Uint32(wav[24:]); rate != captureRate {
		t.Errorf("sample rate = %d", rate)
	}
	if size := binary.LittleEndian.Uint32(wav[40:]); int(size) != len(pcm) {
		t.Errorf("data size = %d", size)
	}
	if !bytes.Equal(wav[44:], pcm) {
		t.Error("payload does not round-trip")
	}
}
