package media

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyqlelabs/factor/internal/tools"
)

type notices struct {
	mu    sync.Mutex
	got   []string
	from  []Origin
	fired chan struct{}
}

func newNotices() *notices { return &notices{fired: make(chan struct{}, 8)} }

func (n *notices) notify(origin Origin, text string) {
	n.mu.Lock()
	n.got = append(n.got, text)
	n.from = append(n.from, origin)
	n.mu.Unlock()
	n.fired <- struct{}{}
}

func (n *notices) wait(t *testing.T) string {
	t.Helper()
	select {
	case <-n.fired:
	case <-time.After(10 * time.Second):
		t.Fatal("no notice arrived")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.got[len(n.got)-1]
}

func (n *notices) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.got)
}

var origin = Origin{Channel: "voice", ChatID: "local", Audience: "shared"}

// The whole point: play answers only once the source has opened and the
// position is moving, and says what is playing.
func TestPlayReportsASourceThatIsActuallyPlaying(t *testing.T) {
	p := newTestPlayer(t, nil)
	ctx := context.Background()
	if p.Sounding() {
		t.Fatal("sounding before anything played")
	}
	st, err := p.Play(ctx, "https://radio.example/stream", false, origin)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StatePlaying || st.Title != "Title of stream" || st.Source != "https://radio.example/stream" {
		t.Errorf("status = %+v", st)
	}
	if !p.Sounding() {
		t.Error("playing but not sounding")
	}
	line := st.String()
	for _, want := range []string{"Playing", "Title of stream", "live stream", "volume 70%", "nothing queued"} {
		if !strings.Contains(line, want) {
			t.Errorf("status line %q lacks %q", line, want)
		}
	}
	if err := p.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !p.Sounding() })
	if got := p.Status(ctx); got.State != StatePaused {
		t.Errorf("after pause: %+v", got)
	}
	if err := p.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p.Sounding)
}

// A source that fails to open is a named failure, not a status to reread.
func TestPlayNamesWhyASourceFailed(t *testing.T) {
	p := newTestPlayer(t, nil)
	_, err := p.Play(context.Background(), "https://radio.example/broken", false, origin)
	if err == nil || !strings.Contains(err.Error(), "Failed to open https://radio.example/broken") {
		t.Fatalf("err = %v", err)
	}
	if p.Sounding() {
		t.Error("a failed source is sounding")
	}
	if st := p.Status(context.Background()); st.State != StateStopped || !strings.Contains(st.String(), "The last source failed") {
		t.Errorf("status = %+v / %s", st, st)
	}
}

// Opened is not playing: a position that never moves is no sound coming out,
// which is exactly what "the process is alive" used to be reported as.
func TestPlayRefusesASourceWhosePositionNeverMoves(t *testing.T) {
	p := newTestPlayer(t, nil)
	_, err := p.Play(context.Background(), "https://radio.example/stuck", false, origin)
	if err == nil || !strings.Contains(err.Error(), "not advancing") {
		t.Fatalf("err = %v", err)
	}
}

// When the queue runs out the conversation that asked for the music is told,
// once, with the last title — and a stop the user asked for is not news.
func TestQueueRunningOutIsAnnouncedOnce(t *testing.T) {
	n := newNotices()
	p := newTestPlayer(t, n.notify)
	ctx := context.Background()
	if _, err := p.Play(ctx, "/music/short-one.mp3", false, origin); err != nil {
		t.Fatal(err)
	}
	st, err := p.Play(ctx, "/music/short-two.mp3", true, origin)
	if err != nil {
		t.Fatal(err)
	}
	if st.Queued != 1 {
		t.Errorf("queued = %+v", st)
	}
	text := n.wait(t)
	for _, want := range []string{"queue is empty", "Nothing is playing now"} {
		if !strings.Contains(text, want) {
			t.Errorf("notice %q lacks %q", text, want)
		}
	}
	n.mu.Lock()
	from := n.from[0]
	n.mu.Unlock()
	if from != origin {
		t.Errorf("notice went to %+v", from)
	}
	if p.Sounding() {
		t.Error("still sounding after the queue drained")
	}

	if _, err := p.Play(ctx, "https://radio.example/stream", false, origin); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !p.Sounding() })
	time.Sleep(200 * time.Millisecond)
	if n.count() != 1 {
		t.Errorf("a deliberate stop was announced: %v", n.got)
	}
}

// The player dying mid-song is the other silence nobody was told about.
func TestPlayerExitingMidPlayIsAnnounced(t *testing.T) {
	n := newNotices()
	p := newTestPlayer(t, n.notify)
	if _, err := p.Play(context.Background(), "/music/crash.mp3", false, origin); err != nil {
		// The fake may die before the advance probe finishes; either way the
		// exit has to be reported.
		t.Log(err)
	}
	text := n.wait(t)
	if !strings.Contains(text, "exited while playing") {
		t.Errorf("notice = %q", text)
	}
	if st := p.Status(context.Background()); st.State != StateOff {
		t.Errorf("after the exit: %+v", st)
	}
	// And the next play starts a fresh player rather than failing on the
	// dead one.
	if _, err := p.Play(context.Background(), "https://radio.example/stream", false, origin); err != nil {
		t.Fatal(err)
	}
}

// Ducking is held by however many things need the room quiet and released
// when the last one lets go; the level the user chose is what comes back.
func TestDuckHoldsUntilEveryHolderReleases(t *testing.T) {
	p := newTestPlayer(t, nil)
	ctx := context.Background()
	if _, err := p.Play(ctx, "https://radio.example/stream", false, origin); err != nil {
		t.Fatal(err)
	}
	if err := p.SetVolume(ctx, 80); err != nil {
		t.Fatal(err)
	}
	p.Duck(true)
	p.Duck(true)
	p.Duck(false)
	if st := p.Status(ctx); !st.Ducked || !strings.Contains(st.String(), "ducked") {
		t.Errorf("released too early: %+v", st)
	}
	p.Duck(false)
	if st := p.Status(ctx); st.Ducked || st.Volume != 80 {
		t.Errorf("not released: %+v", st)
	}
	got := strings.Join(mpvLog(t), " ")
	if !strings.Contains(got, "volume=80 volume=20 volume=80") {
		t.Errorf("mpv saw %q; want the level, its ducked quarter, then the level back", got)
	}
	if err := p.SetVolume(ctx, 101); err == nil {
		t.Error("volume 101 accepted")
	}
}

// A machine without mpv is told what to install, in the words pkg_install
// understands, rather than handed a shell error.
func TestMissingPlayerIsNamedWithItsPackage(t *testing.T) {
	p := NewPlayer(t.TempDir(), nil)
	p.argv = []string{"definitely-not-installed-mpv-xyz"}
	_, err := p.Play(context.Background(), "https://radio.example/stream", false, origin)
	if err == nil || !strings.Contains(err.Error(), "pkg_install") {
		t.Fatalf("err = %v", err)
	}
	if got := MissingHelpers(func(string) bool { return false }); len(got) != 1 || got[0].Bin != "mpv" {
		t.Errorf("missing = %+v", got)
	}
	if got := MissingHelpers(func(string) bool { return true }); len(got) != 0 {
		t.Errorf("missing with mpv present = %+v", got)
	}
	if err := p.Pause(context.Background()); err == nil {
		t.Error("pause with no player succeeded")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("stop with no player must be a no-op: %v", err)
	}
}

func TestStatusStringsCoverEveryState(t *testing.T) {
	cases := map[Status]string{
		{State: StateOff}:                    "Nothing is playing.",
		{State: StateStopped, Error: "boom"}: "The last source failed: boom.",
		{State: StatePlaying, Title: "Song", Position: 65 * time.Second, Duration: time.Hour}: `Playing "Song" at 1:05 of 1:00:00`,
		{State: StateBuffering, Title: "Radio", Queued: 2, Volume: 40}:                        "Buffering \"Radio\"; volume 40%; 2 more queued.",
		{State: StatePaused, Title: "Song", Source: "https://x/y", Output: "pulse"}:           `Paused "Song" from https://x/y; volume 0%; output pulse`,
	}
	for st, want := range cases {
		if got := st.String(); !strings.Contains(got, want) {
			t.Errorf("%+v -> %q, want %q in it", st, got, want)
		}
	}
}

// The tool resolves what the model asks for and refuses what the workspace
// rules refuse, with a schema the registry can validate.
func TestToolResolvesSourcesAndGuardsFiles(t *testing.T) {
	ws := t.TempDir()
	guard := tools.NewPathGuard(ws, true, false, nil)
	p := newTestPlayer(t, nil)
	tool := NewTool(p, guard)
	if res := tool.Execute(context.Background(), map[string]any{"action": "play"}); !res.IsError || !strings.Contains(res.ForLLM, "source is required") {
		t.Errorf("play without source = %+v", res)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp3") // a sibling of the workspace, on every platform
	if res := tool.Execute(context.Background(), map[string]any{"action": "play", "source": outside}); !res.IsError || !strings.Contains(res.ForLLM, "outside workspace") {
		t.Errorf("a file outside the workspace = %+v", res)
	}
	if got, err := tool.resolve("ytsearch: Crown Lands Fearless"); err != nil || got != "ytdl://ytsearch1:Crown Lands Fearless" {
		t.Errorf("ytsearch -> %q %v", got, err)
	}
	local := filepath.Join(ws, "song.mp3")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tool.resolve("song.mp3"); err != nil || got != local {
		t.Errorf("a workspace file -> %q %v", got, err)
	}
	if res := tool.Execute(context.Background(), map[string]any{"action": "status"}); res.IsError || res.ForLLM != "Nothing is playing." {
		t.Errorf("status = %+v", res)
	}
	if res := tool.Execute(context.Background(), map[string]any{"action": "dance"}); !res.IsError {
		t.Errorf("unknown action = %+v", res)
	}
	ctx := tools.WithToolContext(context.Background(), tools.ToolContext{Channel: "voice", ChatID: "local"})
	res := tool.Execute(ctx, map[string]any{"action": "play", "source": "https://radio.example/stream"})
	if res.IsError || !strings.Contains(res.ForLLM, `Playing "Title of stream"`) {
		t.Errorf("play = %+v", res)
	}
	if p.origin != (Origin{Channel: "voice", ChatID: "local"}) {
		t.Errorf("origin = %+v", p.origin)
	}
	res = tool.Execute(ctx, map[string]any{"action": "queue", "source": "https://radio.example/other"})
	if res.IsError || !strings.HasPrefix(res.ForLLM, "Queued https://radio.example/other") {
		t.Errorf("queue = %+v", res)
	}
	res = tool.Execute(ctx, map[string]any{"action": "volume", "volume": 30})
	if res.IsError || !strings.Contains(res.ForLLM, "volume 30%") {
		t.Errorf("volume = %+v", res)
	}
	if res := tool.Execute(ctx, map[string]any{"action": "next"}); res.IsError || !strings.Contains(res.ForLLM, "Title of other") {
		t.Errorf("next = %+v", res)
	}
	if res := tool.Execute(ctx, map[string]any{"action": "stop"}); res.IsError || res.ForLLM != "Nothing is playing." {
		t.Errorf("stop = %+v", res)
	}
	props := tool.Parameters()["properties"].(map[string]any)
	for name, raw := range props {
		prop := raw.(map[string]any)
		if prop["type"] == nil || prop["description"] == nil {
			t.Errorf("property %s lacks a type or description", name)
		}
	}
}

// The real thing, when the machine has it: a generated tone through mpv's
// null output still has to open and advance.
func TestLivePlayerPlaysAFile(t *testing.T) {
	if testing.Short() {
		t.Skip("live mpv")
	}
	mpv, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("mpv is not installed")
	}
	p := NewPlayer(t.TempDir(), nil)
	p.argv = []string{mpv, "--ao=null", "--no-config"}
	t.Cleanup(p.Close)
	file := filepath.Join(t.TempDir(), "tone.wav")
	if err := os.WriteFile(file, sineWAV(3*time.Second), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := p.Play(context.Background(), file, false, origin)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StatePlaying || st.Duration < 2*time.Second || !p.Sounding() {
		t.Errorf("status = %+v", st)
	}
}

// sineWAV is a mono 16-bit 8 kHz tone of the given length.
func sineWAV(d time.Duration) []byte {
	const rate = 8000
	n := int(d.Seconds() * rate)
	data := make([]byte, 44+2*n)
	copy(data, "RIFF")
	putLE32(data[4:], uint32(36+2*n))
	copy(data[8:], "WAVEfmt ")
	putLE32(data[16:], 16)
	data[20], data[22] = 1, 1
	putLE32(data[24:], rate)
	putLE32(data[28:], rate*2)
	data[32], data[34] = 2, 16
	copy(data[36:], "data")
	putLE32(data[40:], uint32(2*n))
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(float64(i)*2*math.Pi*440/rate))
		data[44+2*i], data[45+2*i] = byte(v), byte(v>>8)
	}
	return data
}

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// A play that lands while another is still opening does not leave the first
// caller waiting out the start timeout: it is told its source was displaced.
func TestPlayDisplacedByAnotherPlayIsToldSo(t *testing.T) {
	p := newTestPlayer(t, nil)
	p.mu.Lock()
	first := make(chan error, 1)
	p.pending = first
	p.mu.Unlock()
	if _, err := p.Play(context.Background(), "https://radio.example/stream", false, origin); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-first:
		if err == nil || !strings.Contains(err.Error(), "another source") {
			t.Errorf("displaced waiter got %v", err)
		}
	default:
		t.Error("the displaced waiter was told nothing")
	}
}

// Music asked for while somebody is talking starts at the held level.
func TestPlayUnderADuckStartsQuiet(t *testing.T) {
	p := newTestPlayer(t, nil)
	p.Duck(true)
	if _, err := p.Play(context.Background(), "https://radio.example/stream", false, origin); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(mpvLog(t), " "); !strings.HasSuffix(got, "volume=17") {
		t.Errorf("mpv saw %q; want the ducked level applied on load", got)
	}
	p.Duck(false)
	if got := strings.Join(mpvLog(t), " "); !strings.HasSuffix(got, "volume=70") {
		t.Errorf("mpv saw %q; want the level back", got)
	}
}
