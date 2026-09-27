package media

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ipcConn is one connection to mpv's JSON IPC socket: newline-delimited JSON
// both ways, replies matched to requests by request_id, and everything else
// an event.
type ipcConn struct {
	conn   net.Conn
	events chan ipcEvent
	next   atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan ipcReply
	closed  chan struct{}
	err     error
}

type ipcReply struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	ID    int64           `json:"request_id"`
}

// ipcEvent is what mpv volunteers: end-file carries why, property-change
// carries the new value of something observed.
type ipcEvent struct {
	Event     string          `json:"event"`
	Reason    string          `json:"reason"`
	FileError string          `json:"file_error"`
	Name      string          `json:"name"`
	Data      json.RawMessage `json:"data"`
}

const ipcReplyTimeout = 5 * time.Second

func newIPCConn(conn net.Conn) *ipcConn {
	c := &ipcConn{conn: conn, events: make(chan ipcEvent, 64), pending: map[int64]chan ipcReply{}, closed: make(chan struct{})}
	go c.read()
	return c
}

func (c *ipcConn) read() {
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var probe struct {
			Event string `json:"event"`
			ID    int64  `json:"request_id"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.Event != "" {
			var ev ipcEvent
			_ = json.Unmarshal(line, &ev)
			select {
			case c.events <- ev:
			default: // a reader that stopped draining does not stall replies
			}
			continue
		}
		var reply ipcReply
		_ = json.Unmarshal(line, &reply)
		c.mu.Lock()
		ch := c.pending[reply.ID]
		delete(c.pending, reply.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- reply
		}
	}
	err := scanner.Err()
	if err == nil {
		err = errors.New("the player closed the connection")
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.events)
	close(c.closed)
}

// command sends one mpv command and returns its data. mpv answers every
// command; a socket that stops answering is a player that is gone.
func (c *ipcConn) command(ctx context.Context, args ...any) (json.RawMessage, error) {
	id := c.next.Add(1)
	ch := make(chan ipcReply, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	line, err := json.Marshal(map[string]any{"command": args, "request_id": id})
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	timer := time.NewTimer(ipcReplyTimeout)
	defer timer.Stop()
	select {
	case reply, ok := <-ch:
		if !ok {
			return nil, c.failure()
		}
		if reply.Error != "success" {
			return nil, fmt.Errorf("%s: %s", args[0], reply.Error)
		}
		return reply.Data, nil
	case <-timer.C:
		return nil, fmt.Errorf("%s: the player did not answer", args[0])
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *ipcConn) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *ipcConn) close() { _ = c.conn.Close() }

// property reads one mpv property into out.
func (c *ipcConn) property(ctx context.Context, name string, out any) error {
	data, err := c.command(ctx, "get_property", name)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
