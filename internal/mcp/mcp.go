// Package mcp is a small Model Context Protocol server: JSON-RPC 2.0 over
// newline-delimited messages (MCP's stdio transport), offering tools only.
// The app serves it on a unix socket of the user's own; `bowel mcp` bridges
// an agent's stdio to that socket (Bridge), so agents register it as an
// ordinary stdio server.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// Versions are the protocol versions the server speaks, newest first.
var Versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one tool the server offers.
type Tool struct {
	Name        string
	Title       string
	Description string
	// Input is the JSON Schema of the arguments; nil takes none.
	Input map[string]any
	// Schema, if set, gives the schema each time the tools are listed, for
	// arguments whose choices change while the server runs. Input is then
	// not used.
	Schema func() map[string]any
	// ReadOnly tools change nothing; Destructive ones may undo what the
	// user set up. Both are hints for the agent's own prompts.
	ReadOnly    bool
	Destructive bool
	// Run does the call. args is the arguments object ("{}" when none).
	// An error is reported to the agent as the tool's failure, in words.
	Run func(ctx context.Context, s *Session, args json.RawMessage) (any, error)
}

// InputSchema is the JSON Schema of the tool's arguments as of now.
func (t Tool) InputSchema() map[string]any {
	if t.Schema != nil {
		return t.Schema()
	}
	return t.Input
}

// Server answers MCP requests with its tools.
type Server struct {
	Name, Title, Version string
	// Instructions tell the agent what the server is for.
	Instructions string
	Tools        []Tool
}

// Session is one agent's connection.
type Session struct {
	mu sync.Mutex
	// client is the agent's name and version, as it said in initialize.
	client string
	vals   map[string]any
}

// Get is a value the app keeps for the session, as an approval it gave.
func (s *Session) Get(key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vals[key]
}

// Set keeps a value for the rest of the session.
func (s *Session) Set(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals = map[string]any{}
	}
	s.vals[key] = v
}

// Name is the client's name for people: "an agent" when it gave none.
func (s *Session) Name() string {
	if s == nil {
		return "agent"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == "" {
		return "agent"
	}
	return s.client
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// maxMessage bounds one message; tools take small arguments.
const maxMessage = 4 << 20

// Serve answers the messages read from r on w until r ends or ctx is done.
// Each request runs on its own goroutine, so that one waiting for the
// user's approval does not hold up pings or the agent's cancellations.
func (srv *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	sess := &Session{}
	var (
		wmu      sync.Mutex
		inflight sync.WaitGroup
		cmu      sync.Mutex
		cancels  = map[string]context.CancelFunc{}
	)
	// When the agent's input ends, what it asked is still answered: a
	// client may close its side and read on.
	defer cancel()
	defer inflight.Wait()
	send := func(resp response) {
		resp.JSONRPC = "2.0"
		b, err := json.Marshal(resp)
		if err != nil {
			b, _ = json.Marshal(response{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{Code: -32603, Message: err.Error()}})
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = w.Write(append(b, '\n'))
	}
	go func() { // a reader blocked on a connection ends with it
		<-ctx.Done()
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			var req request
			switch {
			case json.Unmarshal(line, &req) != nil:
				send(response{ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: "parse error"}})
			case req.JSONRPC != "2.0" || req.Method == "":
				if len(req.ID) > 0 { // a response to us, or nonsense: we send no requests
					send(response{ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "invalid request"}})
				}
			case len(req.ID) == 0 || string(req.ID) == "null":
				if req.Method == "notifications/cancelled" {
					var p struct {
						RequestID json.RawMessage `json:"requestId"`
					}
					if json.Unmarshal(req.Params, &p) == nil {
						cmu.Lock()
						if c := cancels[string(p.RequestID)]; c != nil {
							c()
						}
						cmu.Unlock()
					}
				}
				// Other notifications (initialized, …) need nothing.
			default:
				rctx, rcancel := context.WithCancel(ctx)
				key := string(req.ID)
				cmu.Lock()
				cancels[key] = rcancel
				cmu.Unlock()
				inflight.Add(1)
				go func() {
					defer inflight.Done()
					res, rerr := srv.handle(rctx, sess, req)
					cancelled := rctx.Err() != nil // the agent gave up on it, or left
					cmu.Lock()
					delete(cancels, key)
					cmu.Unlock()
					rcancel()
					if cancelled {
						return // no answer to a cancelled request
					}
					send(response{ID: req.ID, Result: res, Error: rerr})
				}()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// readLine is the next message: a line, without its end.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		line = append(line, chunk...)
		if len(line) > maxMessage {
			return nil, fmt.Errorf("mcp: a message over %d bytes", maxMessage)
		}
		if err != nil || !isPrefix {
			return line, err
		}
	}
}

func (srv *Server) handle(ctx context.Context, sess *Session, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Title   string `json:"title"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		_ = json.Unmarshal(req.Params, &p)
		name := p.ClientInfo.Title
		if name == "" {
			name = p.ClientInfo.Name
		}
		if name != "" && p.ClientInfo.Version != "" {
			name += " " + p.ClientInfo.Version
		}
		if name != "" {
			sess.mu.Lock()
			sess.client = name
			sess.mu.Unlock()
		}
		v := Versions[0]
		if slices.Contains(Versions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		info := map[string]any{"name": srv.Name, "version": srv.Version}
		if srv.Title != "" {
			info["title"] = srv.Title
		}
		res := map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      info,
		}
		if srv.Instructions != "" {
			res["instructions"] = srv.Instructions
		}
		return res, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, 0, len(srv.Tools))
		for _, t := range srv.Tools {
			in := t.InputSchema()
			if in == nil {
				in = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": in,
				"annotations": map[string]any{"readOnlyHint": t.ReadOnly, "destructiveHint": t.Destructive, "openWorldHint": false}}
			if t.Title != "" {
				m["title"] = t.Title
				m["annotations"].(map[string]any)["title"] = t.Title
			}
			tools = append(tools, m)
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		i := slices.IndexFunc(srv.Tools, func(t Tool) bool { return t.Name == p.Name })
		if i < 0 {
			return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		out, err := run(ctx, srv.Tools[i], sess, args)
		if err != nil {
			return map[string]any{"content": []any{text(err.Error())}, "isError": true}, nil
		}
		return Result(out), nil
	}
	return nil, &rpcError{Code: codeNoMethod, Message: "method not found: " + req.Method}
}

// run calls t, turning a panic into the tool's failure.
func run(ctx context.Context, t Tool, sess *Session, args json.RawMessage) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return t.Run(ctx, sess, args)
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// Result is a tool's output as MCP returns it: a string as text; anything
// else as indented JSON, and, when it is an object, as structured content
// too.
func Result(out any) map[string]any {
	if s, ok := out.(string); ok {
		return map[string]any{"content": []any{text(s)}}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return map[string]any{"content": []any{text(err.Error())}, "isError": true}
	}
	res := map[string]any{"content": []any{text(string(b))}}
	if len(b) > 0 && b[0] == '{' {
		res["structuredContent"] = json.RawMessage(b)
	}
	return res
}

// Args decodes a tool's arguments into v, refusing names v does not have:
// an agent's typo should fail, not be ignored.
func Args(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}
