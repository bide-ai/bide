package mcptools

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawServer is an in-process MCP server that speaks newline-delimited JSON-RPC by hand, so a test
// can make it do what no well-behaved SDK server does: advertise any tool list (duplicate or
// malformed names, any schema), answer a call with any result, or drop the connection partway
// through a call.
type rawServer struct {
	t     *testing.T
	tools []json.RawMessage // the tools/list result, verbatim
	// call handles tools/call. It answers through c (Reply or Hangup); returning without either
	// leaves the call unanswered.
	call func(c *rawCall)

	mu     sync.Mutex
	w      io.WriteCloser // server to client
	r      io.ReadCloser  // client to server
	closed bool
}

// rawCall is one tools/call request the fake server received.
type rawCall struct {
	s      *rawServer
	id     json.RawMessage
	Name   string
	Args   json.RawMessage
	params json.RawMessage
}

// Reply answers the call with result, marshalled as the JSON-RPC result.
func (c *rawCall) Reply(result any) {
	c.s.send(map[string]any{"jsonrpc": "2.0", "id": c.id, "result": result})
}

// ReplyError answers the call with a JSON-RPC error response.
func (c *rawCall) ReplyError(code int, msg string) {
	c.s.send(map[string]any{"jsonrpc": "2.0", "id": c.id, "error": map[string]any{"code": code, "message": msg}})
}

// Hangup drops the connection without answering, as a server that crashes mid-call does.
func (c *rawCall) Hangup() { c.s.hangup() }

func (s *rawServer) send(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		s.t.Errorf("raw server: marshal: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_, _ = s.w.Write(append(b, '\n'))
}

func (s *rawServer) hangup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		_ = s.w.Close()
		_ = s.r.Close()
	}
}

// connectRaw starts s and returns a client session connected to it through Connect.
func connectRaw(t *testing.T, s *rawServer, opts ...Option) *mcp.ClientSession {
	t.Helper()
	s.t = t
	cr, sw := io.Pipe() // server writes, client reads
	sr, cw := io.Pipe() // client writes, server reads
	s.w, s.r = sw, sr
	go s.serve()
	session, err := Connect(context.Background(), &mcp.IOTransport{Reader: cr, Writer: cw}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.hangup(); session.Close() })
	return session
}

func (s *rawServer) serve() {
	sc := bufio.NewScanner(s.r)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil || len(req.ID) == 0 {
			continue // a notification or a response: nothing to answer
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			s.send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "raw", "version": "0.1.0"},
			}})
		case "tools/list":
			s.send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": s.tools}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			c := &rawCall{s: s, id: req.ID, Name: p.Name, Args: p.Arguments, params: req.Params}
			if s.call != nil {
				go s.call(c)
			}
		default:
			s.send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}

// rawTool is a tools/list entry with an object input schema.
func rawTool(name string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
	return b
}
