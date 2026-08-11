// Package mcp implements a minimal MCP client (JSON-RPC 2.0 over stdio,
// newline-delimited) so external MCP servers' tools mount directly into
// Factor's tool registry as <server>__<tool>.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const protocolVersion = "2025-03-26"

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ToolSpec is one tool advertised by a server.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client is one connected MCP server process.
type Client struct {
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex
	seq     int64

	pendingMu sync.Mutex
	pending   map[int64]chan *rpcResponse

	closed chan struct{}
	once   sync.Once
}

// Connect spawns the server, performs the initialize handshake, and returns
// a ready client.
func Connect(ctx context.Context, name, command string, args []string, env map[string]string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // MCP servers may log to stderr; keep it out of our stdio
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	c := &Client{
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int64]chan *rpcResponse{},
		closed:  make(chan struct{}),
	}
	go c.readLoop(stdout)
	go func() {
		_ = cmd.Wait()
		c.shutdown()
	}()

	initCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = c.request(initCtx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "factor", "version": "1.0"},
	})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp %s initialize: %w", name, err)
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil || resp.ID == nil {
			continue // notification or garbage; ignore
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[*resp.ID]
		delete(c.pending, *resp.ID)
		c.pendingMu.Unlock()
		if ok {
			ch <- &resp
		}
	}
	c.shutdown()
}

func (c *Client) shutdown() {
	c.once.Do(func() {
		close(c.closed)
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			delete(c.pending, id)
			close(ch)
		}
		c.pendingMu.Unlock()
	})
}

func (c *Client) send(msg rpcRequest) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *Client) notify(method string, params any) error {
	return c.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.pendingMu.Lock()
	c.seq++
	id := c.seq
	ch := make(chan *rpcResponse, 1)
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.send(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("mcp server %s exited", c.name)
	case resp, ok := <-ch:
		if !ok || resp == nil {
			return nil, fmt.Errorf("mcp server %s closed mid-request", c.name)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp %s: %s (code %d)", c.name, resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	}
}

// ListTools fetches the server's tool catalog.
func (c *Client) ListTools(ctx context.Context) ([]ToolSpec, error) {
	raw, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []ToolSpec `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool invokes one tool; returns the concatenated text content.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	raw, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", true, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), false, nil // non-standard result; give the model the raw JSON
	}
	text := ""
	for _, block := range out.Content {
		if block.Type == "text" {
			if text != "" {
				text += "\n"
			}
			text += block.Text
		}
	}
	return text, out.IsError, nil
}

// Close terminates the server process.
func (c *Client) Close() error {
	c.shutdown()
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		go func() {
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			<-timer.C
			_ = c.cmd.Process.Kill()
		}()
	}
	return nil
}

// Alive reports whether the server process is still running.
func (c *Client) Alive() bool {
	select {
	case <-c.closed:
		return false
	default:
		return true
	}
}
