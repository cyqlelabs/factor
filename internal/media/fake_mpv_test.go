package media

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain lets this test binary impersonate mpv: it opens the IPC socket it
// was told to, speaks the JSON protocol, and plays sources whose names say how
// they behave — so the player's spawn, wait, verify, notify and shutdown paths
// run against a real child process and a real socket.
func TestMain(m *testing.M) {
	if os.Getenv("FACTOR_TEST_MPV") == "1" {
		fakeMPV()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newTestPlayer builds a player whose mpv is this test binary.
func newTestPlayer(t *testing.T, notify Notifier) *Player {
	t.Helper()
	t.Setenv("FACTOR_TEST_MPV", "1")
	t.Setenv("FACTOR_TEST_MPV_LOG", path.Join(t.TempDir(), "mpv.log"))
	p := NewPlayer(t.TempDir(), notify)
	p.argv = []string{os.Args[0]}
	t.Cleanup(p.Close)
	return p
}

// mpvLog is every set_property the fake received, one "name=value" per line.
func mpvLog(t *testing.T) []string {
	t.Helper()
	data, _ := os.ReadFile(os.Getenv("FACTOR_TEST_MPV_LOG"))
	return strings.Fields(string(data))
}

type fakeState struct {
	mu       sync.Mutex
	playlist []string
	index    int
	loaded   bool
	paused   bool
	loadedAt time.Time
	volume   float64
	out      *bufio.Writer
	logPath  string
}

func fakeMPV() {
	var address string
	for _, arg := range os.Args[1:] {
		if v, ok := strings.CutPrefix(arg, "--input-ipc-server="); ok {
			address = v
		}
	}
	if address == "" {
		fmt.Fprintln(os.Stderr, "fake mpv: no --input-ipc-server")
		os.Exit(4)
	}
	ln, err := listenIPC(address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake mpv:", err)
		os.Exit(4)
	}
	go func() { time.Sleep(2 * time.Minute); os.Exit(0) }() // never outlive the run
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		st := &fakeState{out: bufio.NewWriter(conn), volume: 70, logPath: os.Getenv("FACTOR_TEST_MPV_LOG")}
		st.serve(conn)
	}
}

func (s *fakeState) emit(ev map[string]any) {
	line, _ := json.Marshal(ev)
	s.mu.Lock()
	_, _ = s.out.Write(append(line, '\n'))
	_ = s.out.Flush()
	s.mu.Unlock()
}

func (s *fakeState) reply(id int64, data any, errText string) {
	line, _ := json.Marshal(map[string]any{"request_id": id, "error": errText, "data": data})
	s.mu.Lock()
	_, _ = s.out.Write(append(line, '\n'))
	_ = s.out.Flush()
	s.mu.Unlock()
}

// startCurrent opens the entry at index, behaving as its name says: "broken"
// fails to open, "short" ends after a moment, "stuck" opens but never
// advances, "crash" takes the whole player down.
func (s *fakeState) startCurrent() {
	s.mu.Lock()
	if s.index >= len(s.playlist) {
		s.loaded = false
		s.playlist, s.index = nil, 0
		s.mu.Unlock()
		s.emit(map[string]any{"event": "idle"})
		return
	}
	src := s.playlist[s.index]
	s.mu.Unlock()
	s.emit(map[string]any{"event": "start-file"})
	if strings.Contains(src, "broken") {
		s.emit(map[string]any{"event": "end-file", "reason": "error", "file_error": "Failed to open " + src + "."})
		s.mu.Lock()
		s.index++
		s.mu.Unlock()
		s.startCurrent()
		return
	}
	s.mu.Lock()
	s.loaded, s.loadedAt = true, time.Now()
	s.mu.Unlock()
	s.emit(map[string]any{"event": "file-loaded"})
	switch {
	case strings.Contains(src, "crash"):
		go func() { time.Sleep(300 * time.Millisecond); os.Exit(2) }()
	case strings.Contains(src, "short"):
		go func() {
			time.Sleep(700 * time.Millisecond)
			s.mu.Lock()
			if !s.loaded || s.index >= len(s.playlist) || s.playlist[s.index] != src {
				s.mu.Unlock()
				return
			}
			s.loaded = false
			s.index++
			s.mu.Unlock()
			s.emit(map[string]any{"event": "end-file", "reason": "eof"})
			s.startCurrent()
		}()
	}
}

func (s *fakeState) position() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded || s.index >= len(s.playlist) || strings.Contains(s.playlist[s.index], "stuck") {
		return 0
	}
	return time.Since(s.loadedAt).Seconds()
}

func (s *fakeState) serve(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var req struct {
			Command []any `json:"command"`
			ID      int64 `json:"request_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil || len(req.Command) == 0 {
			continue
		}
		name, _ := req.Command[0].(string)
		switch name {
		case "observe_property":
			s.reply(req.ID, nil, "success")
		case "loadfile":
			src, _ := req.Command[1].(string)
			mode, _ := req.Command[2].(string)
			s.mu.Lock()
			if mode == "append-play" && s.loaded {
				s.playlist = append(s.playlist, src)
				s.mu.Unlock()
				s.reply(req.ID, nil, "success")
				continue
			}
			s.playlist, s.index = []string{src}, 0
			s.loaded = false
			s.mu.Unlock()
			s.reply(req.ID, nil, "success")
			s.startCurrent()
		case "stop":
			s.mu.Lock()
			s.playlist, s.index, s.loaded = nil, 0, false
			s.mu.Unlock()
			s.reply(req.ID, nil, "success")
			s.emit(map[string]any{"event": "end-file", "reason": "stop"})
			s.emit(map[string]any{"event": "idle"})
		case "playlist-next":
			s.mu.Lock()
			s.loaded = false
			s.index++
			s.mu.Unlock()
			s.reply(req.ID, nil, "success")
			s.emit(map[string]any{"event": "end-file", "reason": "stop"})
			s.startCurrent()
		case "set_property":
			prop, _ := req.Command[1].(string)
			s.mu.Lock()
			switch prop {
			case "pause":
				s.paused, _ = req.Command[2].(bool)
			case "volume":
				s.volume, _ = req.Command[2].(float64)
			}
			paused := s.paused
			s.mu.Unlock()
			if f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				fmt.Fprintf(f, "%s=%v\n", prop, req.Command[2])
				_ = f.Close()
			}
			s.reply(req.ID, nil, "success")
			if prop == "pause" {
				s.emit(map[string]any{"event": "property-change", "id": 1, "name": "pause", "data": paused})
			}
		case "get_property":
			prop, _ := req.Command[1].(string)
			s.mu.Lock()
			var src string
			if s.index < len(s.playlist) {
				src = s.playlist[s.index]
			}
			count, index, paused, volume := len(s.playlist), s.index, s.paused, s.volume
			s.mu.Unlock()
			switch prop {
			case "time-pos":
				s.reply(req.ID, s.position(), "success")
			case "duration":
				if strings.Contains(src, "stream") {
					s.reply(req.ID, nil, "property unavailable")
				} else {
					s.reply(req.ID, 240.0, "success")
				}
			case "media-title":
				s.reply(req.ID, "Title of "+path.Base(src), "success")
			case "path":
				s.reply(req.ID, src, "success")
			case "playlist-count":
				s.reply(req.ID, count, "success")
			case "playlist-pos":
				s.reply(req.ID, index, "success")
			case "paused-for-cache":
				s.reply(req.ID, false, "success")
			case "pause":
				s.reply(req.ID, paused, "success")
			case "volume":
				s.reply(req.ID, volume, "success")
			case "current-ao":
				s.reply(req.ID, "fake", "success")
			case "audio-device":
				s.reply(req.ID, "auto", "success")
			default:
				s.reply(req.ID, nil, "property not found")
			}
		case "quit":
			s.reply(req.ID, nil, "success")
			os.Exit(0)
		default:
			s.reply(req.ID, nil, "invalid parameter")
		}
	}
}
