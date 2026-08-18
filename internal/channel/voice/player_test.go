package voice

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeSpeaker is an Env.Play that drains the feeder the way a real helper
// would, recording what it heard. Each call is one helper process.
type fakeSpeaker struct {
	mu    sync.Mutex
	runs  [][]byte
	calls int
}

func (f *fakeSpeaker) play(ctx context.Context, _ []string, pcm io.Reader) error {
	f.mu.Lock()
	f.calls++
	run := len(f.runs)
	f.runs = append(f.runs, nil)
	f.mu.Unlock()

	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := pcm.Read(buf)
		if n > 0 {
			f.mu.Lock()
			f.runs[run] = append(f.runs[run], buf[:n]...)
			f.mu.Unlock()
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (f *fakeSpeaker) heard() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []byte
	for _, run := range f.runs {
		all = append(all, run...)
	}
	return all
}

func (f *fakeSpeaker) processes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// runLength is how many bytes one helper run played, 0 when it never ran.
func (f *fakeSpeaker) runLength(i int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.runs) {
		return 0
	}
	return len(f.runs[i])
}

func speakerAndPlayer() (*fakeSpeaker, *player) {
	speaker := &fakeSpeaker{}
	env := Env{Play: speaker.play}
	return speaker, newPlayer(env, []string{"fake-play"})
}

// clip is small so real-time pacing keeps tests fast: 12000 bytes = 250 ms.
func clip(n int) []byte {
	pcm := make([]byte, n)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	return pcm
}

func TestPlayerPlaysAClipToCompletion(t *testing.T) {
	speaker, p := speakerAndPlayer()
	pcm := clip(12000)
	select {
	case result := <-p.play(context.Background(), pcm):
		if !result.completed {
			t.Error("an uninterrupted clip reported incomplete")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("playback never finished")
	}
	if !bytes.Equal(speaker.heard(), pcm) {
		t.Errorf("heard %d bytes, want the whole %d-byte clip", len(speaker.heard()), len(pcm))
	}
	if p.playing() || p.busy() {
		t.Error("a finished player still reports itself busy")
	}
}

func TestPlayerPauseKeepsThePlaceAndResumePicksItUp(t *testing.T) {
	speaker, p := speakerAndPlayer()
	pcm := clip(96000) // 2 s: long enough to pause in the middle
	done := p.play(context.Background(), pcm)

	waitUntil(t, func() bool { return len(speaker.heard()) > 0 })
	p.pause()
	if p.playing() {
		t.Error("a paused player reports sound")
	}
	if !p.busy() {
		t.Error("a paused player must still hold the floor")
	}
	select {
	case <-done:
		t.Fatal("pause ended the clip; it should only hold it")
	case <-time.After(100 * time.Millisecond):
	}

	p.resume(context.Background())
	select {
	case result := <-done:
		if !result.completed {
			t.Error("a resumed clip that ran to the end reported incomplete")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed clip never finished")
	}
	if speaker.processes() != 2 {
		t.Errorf("pause+resume used %d helper processes, want 2", speaker.processes())
	}
	// The rewind means some audio repeats; the clip's end must be intact.
	if !bytes.HasSuffix(speaker.heard(), pcm[len(pcm)-4096:]) {
		t.Error("the end of the clip was never heard")
	}
}

func TestPlayerStopDiscardsTheRest(t *testing.T) {
	speaker, p := speakerAndPlayer()
	done := p.play(context.Background(), clip(96000))
	waitUntil(t, func() bool { return len(speaker.heard()) > 0 })
	p.stop()
	select {
	case result := <-done:
		if result.completed {
			t.Error("a stopped clip reported completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop never settled the clip")
	}
	if p.busy() {
		t.Error("a stopped player still holds the floor")
	}
	if len(speaker.heard()) >= 96000 {
		t.Error("stop played the whole clip anyway")
	}
}

func TestPlayerReplacingAClipSettlesTheOldOne(t *testing.T) {
	speaker, p := speakerAndPlayer()
	first := p.play(context.Background(), clip(96000))
	waitUntil(t, func() bool { return len(speaker.heard()) > 0 })
	second := p.play(context.Background(), clip(12000))
	select {
	case result := <-first:
		if result.completed {
			t.Error("a replaced clip reported completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacing never settled the first clip")
	}
	select {
	case result := <-second:
		if !result.completed {
			t.Error("the replacing clip did not complete")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second clip never finished")
	}
}

func TestPlayerPauseAndResumeAreSafeWhenIdle(t *testing.T) {
	_, p := speakerAndPlayer()
	p.pause()
	p.resume(context.Background())
	p.stop()
	if p.busy() {
		t.Error("an idle player reports busy")
	}
}

// fakeFileSpeaker is an Env.PlayFile that "plays" each WAV clip for its real
// duration, recording what it was handed. Each call is one helper process.
type fakeFileSpeaker struct {
	mu    sync.Mutex
	clips [][]byte
}

func (f *fakeFileSpeaker) playFile(ctx context.Context, _ []string, wav []byte) error {
	f.mu.Lock()
	f.clips = append(f.clips, wav)
	f.mu.Unlock()
	duration := time.Duration(len(wav)-44) * time.Second / (playbackRate * 2)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(duration):
		return nil
	}
}

func (f *fakeFileSpeaker) clipCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clips)
}

func (f *fakeFileSpeaker) clipLen(i int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.clips) {
		return 0
	}
	return len(f.clips[i]) - 44 // strip the WAV header
}

func filePlayer() (*fakeFileSpeaker, *player) {
	speaker := &fakeFileSpeaker{}
	return speaker, newPlayer(Env{PlayFile: speaker.playFile}, []string{"afplay"})
}

func TestFilePlayerPlaysAClipToCompletion(t *testing.T) {
	speaker, p := filePlayer()
	pcm := clip(12000) // 250 ms
	select {
	case result := <-p.play(context.Background(), pcm):
		if !result.completed {
			t.Error("an uninterrupted clip reported incomplete")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("playback never finished")
	}
	if speaker.clipCount() != 1 || speaker.clipLen(0) != len(pcm) {
		t.Errorf("clips = %d, first = %d bytes", speaker.clipCount(), speaker.clipLen(0))
	}
	if p.busy() {
		t.Error("a finished player still reports busy")
	}
}

// A file-only helper cannot stream, so pause tracks the heard position by
// wall clock and resume hands over only the remainder.
func TestFilePlayerPauseResumeReplaysTheRemainder(t *testing.T) {
	speaker, p := filePlayer()
	pcm := clip(96000) // 2 s
	done := p.play(context.Background(), pcm)

	time.Sleep(500 * time.Millisecond) // hear roughly a quarter of it
	p.pause()
	if !p.busy() || p.playing() {
		t.Error("a paused file player should hold the floor silently")
	}
	p.mu.Lock()
	offset := p.offset
	p.mu.Unlock()
	if offset == 0 || offset >= len(pcm) {
		t.Errorf("paused offset = %d of %d; the wall clock never advanced it", offset, len(pcm))
	}

	p.resume(context.Background())
	select {
	case result := <-done:
		if !result.completed {
			t.Error("a resumed clip that ran to the end reported incomplete")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed clip never finished")
	}
	if speaker.clipCount() != 2 {
		t.Fatalf("pause+resume handed over %d clips, want 2", speaker.clipCount())
	}
	if remainder := speaker.clipLen(1); remainder >= len(pcm) || remainder == 0 {
		t.Errorf("the resume clip is %d bytes; it should be the remainder of %d", remainder, len(pcm))
	}
}

func TestFilePlayerStopDiscardsTheRest(t *testing.T) {
	speaker, p := filePlayer()
	done := p.play(context.Background(), clip(96000))
	waitUntil(t, func() bool { return speaker.clipCount() == 1 })
	p.stop()
	select {
	case result := <-done:
		if result.completed {
			t.Error("a stopped clip reported completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop never settled the clip")
	}
	if p.busy() {
		t.Error("a stopped player still holds the floor")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
