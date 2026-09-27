// Package media plays music and other audio on the machine's speakers through
// one managed mpv, driven over its JSON IPC socket.
//
// It exists because the alternative was measured: asked to play a radio
// station, the agent launched ffplay, then vlc, then paplay, read "the
// process is alive" as "music is playing", counted its own voice's paplay
// stream as the music, and told the user the sound card was broken — for
// eleven minutes, with the speakers working the whole time. A player the
// agent owns answers the questions that were being guessed at: whether the
// source opened, whether playback is advancing, what is playing, what is left
// in the queue, and when it all ran out. And a player Factor owns is one the
// microphone can be told about, so a song is not read as a stranger in the
// room.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cyqlelabs/factor/internal/desktop"
	"github.com/cyqlelabs/factor/internal/proxy"
)

const (
	// startTimeout is how long a source has to open before play gives up on
	// it: a radio stream behind a slow resolver takes several seconds, a
	// YouTube page resolved through yt-dlp takes ten.
	startTimeout = 30 * time.Second
	// advanceProbe is how long play watches the position move before it calls
	// the source playing. An opened file whose position never moves is a
	// player with no audio output, which is the failure that used to be
	// reported as success.
	advanceProbe = 400 * time.Millisecond
	advanceTries = 5
	// duckFactor is what the music drops to while somebody is talking or
	// Factor is speaking: audible under the voice, not competing with it.
	duckFactor    = 0.25
	defaultVolume = 70
	// spawnWait is how long mpv has to open its socket.
	spawnWait = 8 * time.Second
	// stderrKeep bounds what is remembered of mpv's stderr, for the cause of
	// a failure that end-file did not name.
	stderrKeep = 4096
)

// Origin is the conversation a piece of music was asked for in, so the notice
// that the queue ran out lands there and under the same audience.
type Origin struct {
	Channel  string
	ChatID   string
	Audience string
}

// Notifier is told, once, when playback the user asked for has ended on its
// own: the queue drained, or the player failed mid-stream. A deliberate stop
// is not news.
type Notifier func(origin Origin, text string)

// State is where playback stands.
type State string

const (
	StateOff       State = "off"       // no player running
	StateStopped   State = "stopped"   // player up, nothing loaded
	StatePlaying   State = "playing"   // loaded and advancing
	StatePaused    State = "paused"    // loaded, paused by request
	StateBuffering State = "buffering" // loaded, waiting on the network
)

// Status is what the player can say about itself right now.
type Status struct {
	State    State
	Title    string
	Source   string
	Position time.Duration
	Duration time.Duration // zero for a live stream
	Queued   int           // entries after the current one
	Volume   int
	Output   string // the audio output mpv is using
	Ducked   bool
	Error    string // why the last source failed, if it did
}

// String renders the status for the model: one line, every fact it needs to
// answer "what is playing" without guessing.
func (s Status) String() string {
	switch s.State {
	case StateOff, StateStopped:
		line := "Nothing is playing."
		if s.Error != "" {
			line += " The last source failed: " + s.Error + "."
		}
		return line
	}
	var b strings.Builder
	switch s.State {
	case StatePlaying:
		b.WriteString("Playing")
	case StatePaused:
		b.WriteString("Paused")
	case StateBuffering:
		b.WriteString("Buffering")
	}
	if s.Title != "" {
		fmt.Fprintf(&b, " %q", s.Title)
	}
	if s.Source != "" && s.Source != s.Title {
		fmt.Fprintf(&b, " from %s", s.Source)
	}
	if s.Duration > 0 {
		fmt.Fprintf(&b, " at %s of %s", clock(s.Position), clock(s.Duration))
	} else if s.Position > 0 {
		fmt.Fprintf(&b, " at %s (live stream)", clock(s.Position))
	}
	fmt.Fprintf(&b, "; volume %d%%", s.Volume)
	if s.Ducked {
		b.WriteString(" (ducked while somebody is talking)")
	}
	if s.Output != "" {
		fmt.Fprintf(&b, "; output %s", s.Output)
	}
	if s.Queued > 0 {
		fmt.Fprintf(&b, "; %d more queued", s.Queued)
	} else {
		b.WriteString("; nothing queued after it")
	}
	b.WriteString(".")
	return b.String()
}

func clock(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// Player is the one mpv this process owns. It is spawned on the first play
// and lives until Close; a player that dies is respawned by the next play.
type Player struct {
	home   string
	argv   []string // the mpv command; tests point it at a fake
	notify Notifier

	mu       sync.Mutex
	cmd      *exec.Cmd
	conn     *ipcConn
	exited   chan struct{} // closed once the watcher has reaped cmd
	stderr   *ring
	loaded   bool // a file is loaded: file-loaded seen, no end-file since
	paused   bool
	played   bool // something has played since the last idle notice
	stopping bool // a stop the user asked for: its idle is not news
	volume   int  // the volume asked for; mpv holds this or its ducked share
	duckers  int  // how many things want the music low right now
	origin   Origin
	lastErr  string
	title    string
	// pending is who is waiting to hear whether the source just loaded
	// opened or failed.
	pending chan error
}

// NewPlayer prepares a player that will run mpv under home when first asked
// to play. notify may be nil.
func NewPlayer(home string, notify Notifier) *Player {
	return &Player{home: home, argv: []string{"mpv"}, notify: notify, volume: defaultVolume}
}

// helper describes the one program this needs, in the shape the wizard and
// pkg_install provision from.
var helper = desktop.Helper{Bin: "mpv", Purpose: "playing music and audio streams"}

// MissingHelpers is the programs the player needs that the machine lacks.
func MissingHelpers(has func(string) bool) []desktop.Helper {
	if has(helper.Bin) {
		return nil
	}
	return []desktop.Helper{helper}
}

// Available reports whether a player can run here at all: the binary is
// installed and the platform has an IPC transport.
func (p *Player) Available() error {
	if ipcAddress(p.home) == "" {
		return errors.New("the media player is not available on Windows yet")
	}
	if _, err := exec.LookPath(p.argv[0]); err != nil {
		return fmt.Errorf("%s is not installed on this machine, so nothing can be played yet; "+
			"install it with pkg_install (package %q) and try again", helper.Bin, helper.Bin)
	}
	return nil
}

// ensureRunning spawns mpv and connects to its socket if neither is up.
func (p *Player) ensureRunning(ctx context.Context) (*ipcConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		return p.conn, nil
	}
	if err := p.Available(); err != nil {
		return nil, err
	}
	address := ipcAddress(p.home)
	removeIPC(address)
	args := append([]string{}, p.argv[1:]...)
	args = append(args,
		"--idle=yes", "--no-video", "--no-terminal", "--audio-display=no",
		"--input-ipc-server="+address,
		fmt.Sprintf("--volume=%d", p.volume),
		"--msg-level=all=warn")
	cmd := exec.Command(p.argv[0], args...)
	// The player fetches the user's streams: it runs on the shell's own
	// environment, not behind the proxy Factor captures itself through.
	cmd.Env = proxy.Environ()
	stderr := &ring{limit: stderrKeep}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", p.argv[0], err)
	}
	conn, err := waitForSocket(ctx, address, cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	exited := make(chan struct{})
	p.cmd, p.conn, p.stderr, p.exited = cmd, conn, stderr, exited
	p.loaded, p.paused, p.played, p.stopping = false, false, false, false
	// pause and the loaded flag are read off events rather than polled, so
	// Sounding costs nothing on the capture loop's hot path.
	if _, err := conn.command(ctx, "observe_property", 1, "pause"); err != nil {
		slog.Warn("media: could not observe the player", "error", err)
	}
	go p.watch(conn, cmd, exited)
	slog.Info("media player started", "helper", p.argv[0], "pid", cmd.Process.Pid)
	return conn, nil
}

// waitForSocket dials until mpv has opened its socket, or the process has
// already given up.
func waitForSocket(ctx context.Context, address string, cmd *exec.Cmd) (*ipcConn, error) {
	deadline := time.Now().Add(spawnWait)
	for {
		dialCtx, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := dialIPC(dialCtx, address)
		cancel()
		if err == nil {
			return newIPCConn(conn), nil
		}
		if cmd.ProcessState != nil || time.Now().After(deadline) {
			return nil, fmt.Errorf("%s did not open its control socket: %w", cmd.Path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// watch turns mpv's events into state, and its exit into a clean slate. It
// owns the notices: the queue running out and the player dying are the two
// things the user was not told about when they happened.
func (p *Player) watch(conn *ipcConn, cmd *exec.Cmd, exited chan struct{}) {
	defer close(exited)
	for ev := range conn.events {
		p.handleEvent(ev)
	}
	// The one Wait on this process: Close waits on exited instead.
	err := cmd.Wait()
	p.mu.Lock()
	if p.conn != conn {
		p.mu.Unlock()
		return
	}
	p.conn, p.cmd = nil, nil
	wasPlaying := p.loaded && !p.stopping
	p.loaded, p.paused = false, false
	origin := p.origin
	tail := p.stderr.String()
	p.mu.Unlock()
	if !wasPlaying {
		return
	}
	slog.Warn("media player exited while playing", "error", err, "stderr", tail)
	p.tell(origin, "[system] The music player exited while playing: "+firstLine(tail, err)+
		". Nothing is playing now; play again if the user still wants music.")
}

func (p *Player) handleEvent(ev ipcEvent) {
	p.mu.Lock()
	switch ev.Event {
	case "file-loaded":
		p.loaded, p.played, p.lastErr = true, true, ""
		if p.pending != nil {
			p.pending <- nil
			p.pending = nil
		}
	case "end-file":
		p.loaded = false
		if ev.Reason == "error" {
			p.lastErr = ev.FileError
			if p.lastErr == "" {
				p.lastErr = firstLine(p.stderr.String(), errors.New("the source could not be played"))
			}
			if p.pending != nil {
				p.pending <- errors.New(p.lastErr)
				p.pending = nil
			}
		}
	case "property-change":
		if ev.Name == "pause" {
			_ = json.Unmarshal(ev.Data, &p.paused)
		}
	case "idle":
		// The playlist is spent. Deliberately, when the user said stop;
		// otherwise it is the moment the room goes quiet with nobody told
		// why — the case that used to be discovered by "no se escucha nada".
		news := p.played && !p.stopping
		p.played, p.stopping = false, false
		if news {
			origin, title, why := p.origin, p.title, p.lastErr
			p.mu.Unlock()
			line := "[system] Music playback ended: the queue is empty"
			if title != "" {
				line += fmt.Sprintf(" (last played %q)", title)
			}
			if why != "" {
				line += "; the last source failed: " + why
			}
			p.tell(origin, line+". Nothing is playing now. Tell the user briefly, and put something else on only if they asked for continuous music.")
			return
		}
	}
	p.mu.Unlock()
}

func (p *Player) tell(origin Origin, text string) {
	if p.notify != nil {
		p.notify(origin, text)
	}
}

// Play replaces whatever is playing with source, or appends it when queue is
// set, and returns only once the source has demonstrably opened and is
// advancing — or with the reason it did not.
func (p *Player) Play(ctx context.Context, source string, queue bool, origin Origin) (Status, error) {
	conn, err := p.ensureRunning(ctx)
	if err != nil {
		return p.Status(ctx), err
	}
	p.mu.Lock()
	p.origin = origin
	p.stopping = false
	busy := p.loaded
	mode := "replace"
	var pending chan error
	if queue && busy {
		mode = "append-play"
	} else {
		// One waiter at a time: a play that lands on top of another takes
		// over the slot, and the earlier caller hears its source was
		// replaced through the status it gets back.
		pending = make(chan error, 1)
		p.pending = pending
	}
	p.mu.Unlock()
	if _, err := conn.command(ctx, "loadfile", source, mode); err != nil {
		p.clearPending(pending)
		return p.Status(ctx), err
	}
	if pending == nil {
		return p.Status(ctx), nil // queued behind what is playing; no opening to wait for
	}
	if err := p.awaitOpen(ctx, conn, source, pending); err != nil {
		return p.Status(ctx), err
	}
	return p.Status(ctx), nil
}

// awaitOpen is the wait between asking for a source and being able to say it
// plays: the open, or its failure, then the position moving.
func (p *Player) awaitOpen(ctx context.Context, conn *ipcConn, source string, pending chan error) error {
	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	select {
	case err := <-pending:
		if err != nil {
			return fmt.Errorf("could not play %s: %s", source, err.Error())
		}
	case <-timer.C:
		p.clearPending(pending)
		return fmt.Errorf("%s did not open within %s: the player is still waiting on it", source, startTimeout)
	case <-ctx.Done():
		p.clearPending(pending)
		return ctx.Err()
	}
	if err := p.advancing(ctx, conn); err != nil {
		return err
	}
	var title string
	_ = conn.property(ctx, "media-title", &title)
	p.mu.Lock()
	p.title = title
	p.mu.Unlock()
	return nil
}

func (p *Player) clearPending(pending chan error) {
	p.mu.Lock()
	if p.pending == pending {
		p.pending = nil
	}
	p.mu.Unlock()
}

// advancing is the check that separates a source that opened from one that
// is producing sound: the playback position has to move. A network stream
// still filling its cache is given the whole probe to start.
func (p *Player) advancing(ctx context.Context, conn *ipcConn) error {
	var first, pos float64
	_ = conn.property(ctx, "time-pos", &first)
	for i := 0; i < advanceTries; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(advanceProbe):
		}
		if err := conn.property(ctx, "time-pos", &pos); err != nil {
			return fmt.Errorf("the source opened but the player stopped answering: %w", err)
		}
		if pos > first {
			return nil
		}
		p.mu.Lock()
		gone := !p.loaded
		p.mu.Unlock()
		if gone {
			break
		}
	}
	var caching bool
	_ = conn.property(ctx, "paused-for-cache", &caching)
	if caching {
		return errors.New("the source opened but is still buffering and has not produced sound yet; " +
			"check status again in a few seconds before calling it playing")
	}
	return errors.New("the source opened but playback is not advancing, so no sound is coming out: " +
		firstLine(p.stderrTail(), errors.New("the audio output may have failed to open")))
}

func (p *Player) stderrTail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stderr == nil {
		return ""
	}
	return p.stderr.String()
}

// Status reads where playback stands. Off when no player is running; the
// rest is asked of mpv, which is the only thing that knows.
func (p *Player) Status(ctx context.Context) Status {
	p.mu.Lock()
	conn := p.conn
	st := Status{State: StateOff, Volume: p.volume, Ducked: p.duckers > 0, Error: p.lastErr}
	loaded, paused := p.loaded, p.paused
	p.mu.Unlock()
	if conn == nil {
		return st
	}
	if !loaded {
		st.State = StateStopped
		return st
	}
	st.State = StatePlaying
	if paused {
		st.State = StatePaused
	} else {
		var caching bool
		if err := conn.property(ctx, "paused-for-cache", &caching); err == nil && caching {
			st.State = StateBuffering
		}
	}
	_ = conn.property(ctx, "media-title", &st.Title)
	_ = conn.property(ctx, "path", &st.Source)
	var pos, dur float64
	_ = conn.property(ctx, "time-pos", &pos)
	_ = conn.property(ctx, "duration", &dur)
	st.Position = time.Duration(pos * float64(time.Second))
	st.Duration = time.Duration(dur * float64(time.Second))
	var count, index int
	_ = conn.property(ctx, "playlist-count", &count)
	_ = conn.property(ctx, "playlist-pos", &index)
	if count > index+1 {
		st.Queued = count - index - 1
	}
	_ = conn.property(ctx, "current-ao", &st.Output)
	var device string
	if err := conn.property(ctx, "audio-device", &device); err == nil && device != "" && device != "auto" {
		st.Output += " " + device
	}
	return st
}

// Pause and Resume hold and release playback; Next skips to the queued entry.
func (p *Player) Pause(ctx context.Context) error  { return p.setPaused(ctx, true) }
func (p *Player) Resume(ctx context.Context) error { return p.setPaused(ctx, false) }

func (p *Player) setPaused(ctx context.Context, paused bool) error {
	if err := p.set(ctx, "pause", paused); err != nil {
		return err
	}
	// Ahead of the property-change event, for the same reason Stop is.
	p.mu.Lock()
	p.paused = paused
	p.mu.Unlock()
	return nil
}

// Next skips to the queued entry and, like Play, answers once it is open.
func (p *Player) Next(ctx context.Context) error {
	conn, err := p.running()
	if err != nil {
		return err
	}
	p.mu.Lock()
	pending := make(chan error, 1)
	p.pending = pending
	p.mu.Unlock()
	if _, err := conn.command(ctx, "playlist-next", "force"); err != nil {
		p.clearPending(pending)
		return err
	}
	return p.awaitOpen(ctx, conn, "the next entry", pending)
}

// Stop empties the queue and silences the player. It is the one ending that
// is not announced: the user asked for it.
func (p *Player) Stop(ctx context.Context) error {
	p.mu.Lock()
	conn := p.conn
	p.stopping = true
	p.mu.Unlock()
	if conn == nil {
		return nil
	}
	if _, err := conn.command(ctx, "stop"); err != nil {
		return err
	}
	// The end-file that follows arrives on the event stream a beat later;
	// the status read straight after a stop must already say stopped.
	p.mu.Lock()
	p.loaded = false
	p.mu.Unlock()
	return nil
}

// SetVolume sets the level the user hears when nobody is talking over it.
func (p *Player) SetVolume(ctx context.Context, level int) error {
	if level < 0 || level > 100 {
		return errors.New("volume is 0 to 100")
	}
	p.mu.Lock()
	p.volume = level
	conn, want := p.conn, p.heldVolume()
	p.mu.Unlock()
	if conn == nil {
		return nil
	}
	return p.apply(ctx, conn, want)
}

// Duck lowers the music while something else has to be heard — the user
// talking, Factor answering — and Duck(false) lets it back up once every
// holder has released it. It is what makes a spoken command over music
// transcribable, and it is the courtesy a person changing the record would
// extend.
func (p *Player) Duck(on bool) {
	p.mu.Lock()
	before := p.heldVolume()
	if on {
		p.duckers++
	} else if p.duckers > 0 {
		p.duckers--
	}
	conn, want, loaded := p.conn, p.heldVolume(), p.loaded
	p.mu.Unlock()
	if conn == nil || !loaded || want == before {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.apply(ctx, conn, want); err != nil {
		slog.Debug("media: could not duck", "error", err)
	}
}

// heldVolume is what mpv should be holding right now. Called with p.mu held.
func (p *Player) heldVolume() int {
	if p.duckers > 0 {
		return int(float64(p.volume) * duckFactor)
	}
	return p.volume
}

func (p *Player) apply(ctx context.Context, conn *ipcConn, volume int) error {
	_, err := conn.command(ctx, "set_property", "volume", volume)
	return err
}

// Sounding reports whether music is coming out of the speakers right now:
// the microphone's question, asked on every frame it captures.
func (p *Player) Sounding() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil && p.loaded && !p.paused
}

func (p *Player) set(ctx context.Context, name string, value any) error {
	conn, err := p.running()
	if err != nil {
		return err
	}
	_, err = conn.command(ctx, "set_property", name, value)
	return err
}

func (p *Player) running() (*ipcConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil || !p.loaded {
		return nil, errors.New("nothing is playing")
	}
	return p.conn, nil
}

// Close ends the player. Playback is not something to outlive the process
// that can control it.
func (p *Player) Close() {
	p.mu.Lock()
	conn, cmd, exited := p.conn, p.cmd, p.exited
	p.conn, p.cmd = nil, nil
	p.loaded, p.stopping = false, true
	p.mu.Unlock()
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = conn.command(ctx, "quit")
	conn.close()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
	removeIPC(ipcAddress(p.home))
}

// firstLine is the first non-empty line of text, or the fallback error.
func firstLine(text string, fallback error) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback.Error()
}

// ring keeps the tail of what is written to it.
type ring struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (r *ring) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, b...)
	if len(r.buf) > r.limit {
		r.buf = r.buf[len(r.buf)-r.limit:]
	}
	return len(b), nil
}

func (r *ring) String() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
