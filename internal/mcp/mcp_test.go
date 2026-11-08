package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// client talks to a server over pipes, a message a line.
type client struct {
	t   *testing.T
	w   io.WriteCloser
	r   *bufio.Scanner
	seq int
}

func start(t *testing.T, srv *Server) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not end when its input did")
		}
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(nil, 1<<20)
	return &client{t: t, w: inW, r: sc}
}

func (c *client) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) read() map[string]any {
	c.t.Helper()
	if !c.r.Scan() {
		c.t.Fatalf("no answer: %v", c.r.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(c.r.Bytes(), &m); err != nil {
		c.t.Fatalf("%v: %s", err, c.r.Bytes())
	}
	return m
}

func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()
	c.seq++
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.seq, "method": method, "params": params})
	m := c.read()
	if id, _ := m["id"].(float64); int(id) != c.seq {
		c.t.Fatalf("answer to %v, want %d: %v", m["id"], c.seq, m)
	}
	return m
}

func testServer(release chan struct{}) *Server {
	return &Server{
		Name: "t", Version: "1", Instructions: "hi",
		Tools: []Tool{
			{Name: "echo", ReadOnly: true, Input: map[string]any{"type": "object"},
				Run: func(ctx context.Context, s *Session, raw json.RawMessage) (any, error) {
					var p struct{ Say string }
					if err := Args(raw, &p); err != nil {
						return nil, err
					}
					return map[string]any{"said": p.Say, "client": s.Name()}, nil
				}},
			{Name: "text", Run: func(context.Context, *Session, json.RawMessage) (any, error) { return "plain", nil }},
			{Name: "fail", Run: func(context.Context, *Session, json.RawMessage) (any, error) { return nil, errors.New("no way") }},
			{Name: "panic", Run: func(context.Context, *Session, json.RawMessage) (any, error) { panic("boom") }},
			{Name: "wait", Run: func(ctx context.Context, _ *Session, _ json.RawMessage) (any, error) {
				select {
				case <-release:
					return "released", nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}},
		},
	}
}

func TestProtocol(t *testing.T) {
	c := start(t, testServer(make(chan struct{})))

	init := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "claude-code", "version": "2.1"}})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["instructions"] != "hi" {
		t.Fatalf("initialize: %v", res)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("no tools capability: %v", res)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}) // no answer

	if r := c.call("initialize", map[string]any{"protocolVersion": "1999-01-01"}); r["result"].(map[string]any)["protocolVersion"] != Versions[0] {
		t.Errorf("an unknown version should get ours: %v", r)
	}
	if r := c.call("ping", nil); r["error"] != nil {
		t.Errorf("ping: %v", r)
	}

	tools := c.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 5 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools: %v", tools)
	}
	if ann := tools[0].(map[string]any)["annotations"].(map[string]any); ann["readOnlyHint"] != true {
		t.Errorf("annotations: %v", ann)
	}
	if in := tools[1].(map[string]any)["inputSchema"].(map[string]any); in["type"] != "object" {
		t.Errorf("a tool without input still needs an object schema: %v", in)
	}

	r := c.call("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"say": "hello"}})["result"].(map[string]any)
	if r["isError"] == true || r["structuredContent"].(map[string]any)["said"] != "hello" {
		t.Errorf("echo: %v", r)
	}
	if r["structuredContent"].(map[string]any)["client"] != "claude-code 2.1" {
		t.Errorf("the session should know its client: %v", r)
	}
	if !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), `"hello"`) {
		t.Errorf("echo's text: %v", r)
	}
	r = c.call("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"sya": "typo"}})["result"].(map[string]any)
	if r["isError"] != true {
		t.Errorf("an unknown argument should fail: %v", r)
	}
	r = c.call("tools/call", map[string]any{"name": "text"})["result"].(map[string]any)
	if r["content"].([]any)[0].(map[string]any)["text"] != "plain" || r["structuredContent"] != nil {
		t.Errorf("text: %v", r)
	}
	r = c.call("tools/call", map[string]any{"name": "fail"})["result"].(map[string]any)
	if r["isError"] != true || r["content"].([]any)[0].(map[string]any)["text"] != "no way" {
		t.Errorf("fail: %v", r)
	}
	r = c.call("tools/call", map[string]any{"name": "panic"})["result"].(map[string]any)
	if r["isError"] != true {
		t.Errorf("a panic is the tool's failure: %v", r)
	}
	if e := c.call("tools/call", map[string]any{"name": "nope"})["error"]; e == nil {
		t.Error("an unknown tool is an error")
	}
	if e := c.call("resources/list", nil)["error"].(map[string]any); e["code"].(float64) != codeNoMethod {
		t.Errorf("resources/list: %v", e)
	}

	if _, err := c.w.Write([]byte("{not json\n")); err != nil {
		t.Fatal(err)
	}
	if e := c.read()["error"].(map[string]any); e["code"].(float64) != codeParse {
		t.Errorf("parse error: %v", e)
	}
}

// TestCancel: a slow call does not hold up others, and a cancelled one is
// not answered.
func TestCancel(t *testing.T) {
	release := make(chan struct{})
	c := start(t, testServer(release))
	c.send(map[string]any{"jsonrpc": "2.0", "id": "slow", "method": "tools/call", "params": map[string]any{"name": "wait"}})
	if r := c.call("ping", nil); r["error"] != nil {
		t.Fatalf("ping behind a waiting call: %v", r)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": "slow"}})
	// The next answer is the ping's, not the cancelled call's.
	if r := c.call("ping", nil); r["error"] != nil {
		t.Fatalf("ping: %v", r)
	}

	c.send(map[string]any{"jsonrpc": "2.0", "id": "slow2", "method": "tools/call", "params": map[string]any{"name": "wait"}})
	close(release)
	r := c.read()
	if r["id"] != "slow2" || r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "released" {
		t.Errorf("released: %v", r)
	}
}

func TestSocketAndBridge(t *testing.T) {
	dir, err := os.MkdirTemp("", "mcp") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")

	// Nothing listens, and nothing can be started.
	if err := Bridge(path, strings.NewReader(""), io.Discard, func() error { return errors.New("nope") }); err == nil {
		t.Error("a bridge with no app should fail")
	}

	// A socket a crash left behind is replaced.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: %v %v", fi.Mode(), err)
	}
	if _, err := Listen(path); !errors.Is(err, ErrRunning) {
		t.Errorf("a second listener: %v, want ErrRunning", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go testServer(nil).Accept(ctx, l)

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"text"}}` + "\n")
	var out strings.Builder
	errc := make(chan error, 1)
	go func() { errc <- Bridge(path, in, &out, nil) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge did not end with its input")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(out.String(), `"plain"`) {
		t.Errorf("through the bridge: %q", out.String())
	}

	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(path); err == nil {
		t.Error("the socket should go when the server stops")
	}
}

// A tool whose arguments depend on what the app knows says it when listed,
// not when the server was built.
func TestSchemaIsReadWhenListed(t *testing.T) {
	choices := []string{"a"}
	c := start(t, &Server{Name: "t", Version: "1", Tools: []Tool{{
		Name:   "pick",
		Schema: func() map[string]any { return map[string]any{"type": "object", "enum": choices} },
	}}})
	enum := func() []any {
		tools := c.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
		return tools[0].(map[string]any)["inputSchema"].(map[string]any)["enum"].([]any)
	}
	if got := enum(); len(got) != 1 {
		t.Fatalf("enum %v", got)
	}
	choices = []string{"a", "b"}
	if got := enum(); len(got) != 2 {
		t.Errorf("the schema was not read again: %v", got)
	}
}
