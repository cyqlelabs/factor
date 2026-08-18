package voice

import (
	"context"
	"io"
	"testing"
	"time"
)

// The live test follows grid_live_test.go's contract: real helpers against
// the machine's real sound system, skipping — never failing — wherever the
// machine cannot host it. The fake-driven tests carry coverage everywhere;
// this one proves the actual helper invocations work: the capture command
// yields well-formed frames at roughly real time, and the playback command
// accepts our PCM format and exits cleanly. It plays 300 ms of silence, so a
// developer's `make check` stays inaudible.
func TestLiveCaptureAndPlayback(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	env := DefaultEnv()
	if !MachineHasAudio(env) {
		t.Skip("no sound system on this machine")
	}
	captureArgv, err := captureCommand(env, "")
	if err != nil {
		t.Skipf("no capture helper: %v", err)
	}
	playbackArgv, err := playbackCommand(env, "")
	if err != nil {
		t.Skipf("no playback helper: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Half a second from the real microphone. A helper that starts but has
	// no default source (a sound card with nothing plugged in, a container's
	// stub ALSA) ends the stream early — that is this machine's problem, not
	// the code's, so it skips.
	stream, err := env.Capture(ctx, captureArgv)
	if err != nil {
		t.Skipf("capture helper %s failed to start: %v", captureArgv[0], err)
	}
	defer stream.Close()
	start := time.Now()
	frame := make([]byte, frameBytes)
	for read := 0; read < 16; read++ { // 16 frames ≈ 480 ms
		if _, err := io.ReadFull(stream, frame); err != nil {
			t.Skipf("the capture stream ended after %d frames: %v", read, err)
		}
	}
	// Real capture cannot outrun the clock: 480 ms of audio arriving near
	// instantly would mean the helper is not honouring the requested rate.
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Errorf("16 frames arrived in %v; the helper is not capturing at 16 kHz", elapsed)
	}

	// 300 ms of silence through the real playback path, via the player so
	// pacing, completion, and process handling all run for real.
	p := newPlayer(env, playbackArgv)
	silence := make([]byte, playbackRate*2*300/1000)
	select {
	case result := <-p.play(ctx, silence):
		if !result.completed {
			t.Errorf("the silent clip did not complete on %s", playbackArgv[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("playback through %s never finished", playbackArgv[0])
	}
}
