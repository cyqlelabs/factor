package voice

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"
)

// player owns the speakers: one clip at a time, fed to the playback helper at
// real-time pace so its position tracks what has actually been heard. That
// pacing is what makes barge-in honest — pause kills the helper and keeps the
// offset, resume starts a new helper a moment before it, and stop discards
// the rest. Replies and proactive messages all pass through here, so there is
// exactly one voice.

// resumeRewind is how far resume steps back: the helper had a little audio
// buffered when it was killed, and losing half a word reads worse than
// hearing it twice.
const resumeRewind = playbackRate * 2 * 300 / 1000 // 300 ms

// playbackTail is how long the speakers still count as speaking after the
// last sample was handed over. The helper exits when the sound server has the
// audio, not when the room has finished hearing it: there is a sink buffer
// behind it and a room in front of it. A microphone segment opening in that
// window is the reply coming back, and calling the floor free there is what
// let the tail of a sentence be heard as somebody talking.
const playbackTail = 400 * time.Millisecond

// playResult reports how one clip ended. completed is true only when the
// whole clip was heard — a stopped or replaced clip reports false, which is
// what tells a multi-part reply to stop mid-list.
type playResult struct {
	completed bool
}

type player struct {
	env  Env
	argv []string
	// fileBased marks a helper that can only read files (afplay): clips go
	// out whole, and the heard position is tracked by wall clock instead of
	// by pacing the stream.
	fileBased bool

	mu     sync.Mutex
	gen    int // one per clip
	runSeq int // one per helper process; pause/resume advances it
	pcm    []byte
	offset int
	active bool
	paused bool
	// tone marks the clip as a sound rather than the agent's voice. It
	// outlives the clip, because playbackTail does: what is fading out of
	// the speakers is still not something anyone can interrupt.
	tone   bool
	cancel context.CancelFunc
	done   chan playResult
	// quietAt is when the last sample was handed over, which playbackTail is
	// measured from.
	quietAt time.Time
}

func newPlayer(env Env, argv []string) *player {
	return &player{env: env, argv: argv, fileBased: len(argv) > 0 && argv[0] == "afplay"}
}

// play replaces whatever is playing with pcm and returns the channel that
// reports how it ended.
func (p *player) play(ctx context.Context, pcm []byte) <-chan playResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endLocked(false)
	p.gen++
	p.tone = false
	p.pcm, p.offset = pcm, 0
	p.done = make(chan playResult, 1)
	p.startLocked(ctx)
	return p.done
}

// chime plays a short tone, but only over silence: the speakers belong to a
// reply that is playing or merely paused, and a courtesy that talks over the
// answer is not one. It reports whether the tone went out. Nothing waits for
// it to finish — a tone nobody is listening for has no ending worth reporting.
func (p *player) chime(ctx context.Context, pcm []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active || p.paused {
		return false
	}
	p.gen++
	p.tone = true
	p.pcm, p.offset = pcm, 0
	p.done = nil
	p.startLocked(ctx)
	return true
}

// pause kills the helper mid-clip but keeps the position; resume picks it
// back up. Both are no-ops when they do not apply.
func (p *player) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return
	}
	p.active, p.paused, p.quietAt = false, true, time.Now()
	p.cancel()
	p.cancel = nil
	if p.offset > resumeRewind {
		p.offset -= resumeRewind
	} else {
		p.offset = 0
	}
}

func (p *player) resume(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.paused {
		return
	}
	p.paused = false
	p.startLocked(ctx)
}

// stop discards the current clip entirely.
func (p *player) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endLocked(false)
}

// sound reports what is in the air right now. playing covers any sound at
// all — the signal the segmenter raises its bar against, and the one that
// marks an utterance as carrying the agent's own voice; speaking narrows it
// to the agent's voice, the only thing there is to interrupt. Both stay true
// through playbackTail past the end of a clip, because the audio outlives the
// helper that fed it.
func (p *player) sound() (playing, speaking bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	playing = p.active || time.Since(p.quietAt) < playbackTail
	return playing, playing && !p.tone
}

func (p *player) playing() bool {
	out, _ := p.sound()
	return out
}

// busy reports whether a clip is underway at all, paused included.
func (p *player) busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active || p.paused
}

// startLocked spawns a helper for the current clip from the current offset.
// Each helper process gets its own run id: a paused run's goroutine can wake
// up after a resume has already started the next one, and must not be allowed
// to settle a clip it no longer owns.
func (p *player) startLocked(ctx context.Context) {
	if p.fileBased {
		p.startFileLocked(ctx)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.active = true
	p.runSeq++
	gen, run := p.gen, p.runSeq
	feeder := &pcmFeeder{p: p, gen: gen, run: run, started: time.Now(), budget: p.offset}
	go func() {
		defer cancel()
		err := p.env.Play(ctx, p.argv, feeder)
		if err != nil && ctx.Err() == nil {
			// A helper that dies on its own is a real failure — a wrong
			// device, a stopped sound server — and staying quiet about it is
			// how a mute agent goes undiagnosed.
			slog.Warn("speech playback failed", "helper", p.argv[0], "error", err)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		// Replaced, stopped, paused, or superseded by a resume: someone else
		// owns the ending.
		if gen != p.gen || run != p.runSeq || !p.active {
			return
		}
		p.endLocked(p.offset >= len(p.pcm))
	}()
}

// startFileLocked is startLocked for a file-only helper: the remainder of the
// clip is handed over whole, and the offset advances with the wall clock so a
// pause still knows how much was heard.
func (p *player) startFileLocked(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.active = true
	p.runSeq++
	gen, run := p.gen, p.runSeq
	clip := p.pcm[p.offset:]
	base := p.offset
	started := time.Now()

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.mu.Lock()
				if gen == p.gen && run == p.runSeq && p.active {
					heard := base + int(time.Since(started).Seconds()*playbackRate*2)
					// Sample-aligned, or a resume plays byte-swapped static.
					p.offset = min(heard, len(p.pcm)) &^ 1
				}
				p.mu.Unlock()
			}
		}
	}()
	go func() {
		defer cancel()
		err := p.env.PlayFile(ctx, p.argv, wavPCM(clip, playbackRate))
		if err != nil && ctx.Err() == nil {
			slog.Warn("speech playback failed", "helper", p.argv[0], "error", err)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if gen != p.gen || run != p.runSeq || !p.active {
			return
		}
		if err == nil {
			p.offset = len(p.pcm)
		}
		p.endLocked(p.offset >= len(p.pcm))
	}()
}

// endLocked settles the current clip and reports how it went.
func (p *player) endLocked(completed bool) {
	if p.active {
		p.quietAt = time.Now()
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.done != nil && (p.active || p.paused) {
		p.done <- playResult{completed: completed}
		p.done = nil
	}
	p.active, p.paused = false, false
	p.pcm, p.offset = nil, 0
}

// pcmFeeder hands the helper its stdin at real-time pace, with a little
// head start so the stream never starves. The pacing is the point: the
// player's offset must mean "heard up to here", not "buffered up to here".
type pcmFeeder struct {
	p       *player
	gen     int
	run     int
	started time.Time
	// budget is where this feeder began, so pacing is relative to its own
	// start rather than the whole clip's.
	budget int
}

// feederLead is how much audio the helper may hold ahead of real time.
const feederLead = playbackRate * 2 * 250 / 1000 // 250 ms

func (f *pcmFeeder) Read(b []byte) (int, error) {
	for {
		f.p.mu.Lock()
		if f.gen != f.p.gen || f.run != f.p.runSeq || !f.p.active {
			f.p.mu.Unlock()
			return 0, io.EOF
		}
		remaining := len(f.p.pcm) - f.p.offset
		if remaining <= 0 {
			f.p.mu.Unlock()
			return 0, io.EOF
		}
		allowed := int(time.Since(f.started).Seconds()*playbackRate*2) + feederLead - (f.p.offset - f.budget)
		if allowed > 0 {
			n := min(len(b), remaining)
			n = min(n, max(allowed, frameBytes))
			// Never split a sample: an offset resting on an odd byte turns
			// every s16 sample after a resume into byte-swapped static.
			if n &^= 1; n == 0 {
				n = min(2, remaining)
			}
			copy(b, f.p.pcm[f.p.offset:f.p.offset+n])
			f.p.offset += n
			f.p.mu.Unlock()
			return n, nil
		}
		f.p.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
}
