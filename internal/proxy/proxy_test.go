package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake server is this test binary re-executed with LAZYMCP_FAKE_SERVER=1. It
// mimics what chrome-devtools-mcp 1.3.0 did in a capture against Claude Code 2.1.287:
// -32601 for server/discover, echoes the client's protocolVersion, asks roots/list
// after initialized, numbers its own requests from 0.
func TestMain(m *testing.M) {
	if os.Getenv("LAZYMCP_FAKE_SERVER") == "1" {
		fakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeCommand is the wrapped command line in every test. It is also the cache key,
// so it must be the same everywhere.
var fakeCommand = []string{os.Args[0], "-test.run=^$"}

func fakeServer() {
	if f := os.Getenv("LAZYMCP_FAKE_STARTS"); f != "" {
		fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fh.WriteString("start\n")
		fh.Close()
	}
	var seen *os.File
	if f := os.Getenv("LAZYMCP_FAKE_SEEN"); f != "" {
		seen, _ = os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	}
	tools := os.Getenv("LAZYMCP_FAKE_TOOLS")
	if tools == "" {
		tools = `[{"name":"evaluate_script","inputSchema":{"type":"object"}}]`
	}
	out := bufio.NewWriter(os.Stdout)
	send := func(s string) { out.WriteString(s + "\n"); out.Flush() }
	nextID := 0
	eachLine(os.Stdin, func(line []byte) bool {
		if seen != nil {
			seen.Write(append(line, '\n'))
		}
		var m message
		if json.Unmarshal(line, &m) != nil {
			return true
		}
		id := string(m.ID)
		switch m.Method {
		case "server/discover":
			send(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32601,"message":"Method not found"}}`)
		case "initialize":
			send(`{"result":{"protocolVersion":"` + protocolVersionOf(m.Params) + `","capabilities":{"logging":{},"tools":{"listChanged":true}},"serverInfo":{"name":"fake","version":"1"}},"jsonrpc":"2.0","id":` + id + `}`)
		case "notifications/initialized":
			send(`{"method":"roots/list","jsonrpc":"2.0","id":` + strconv.Itoa(nextID) + `}`)
			nextID++
		case "tools/list":
			send(`{"result":{"tools":` + tools + `},"jsonrpc":"2.0","id":` + id + `}`)
		case "logging/setLevel":
			send(`{"result":{},"jsonrpc":"2.0","id":` + id + `}`)
		case "tools/call":
			// Echo the raw arguments back inside a text result, the way
			// evaluate_script returns what the page computed.
			var p struct {
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(m.Params, &p)
			text, _ := json.Marshal(string(p.Arguments))
			send(`{"result":{"content":[{"type":"text","text":` + string(text) + `}]},"jsonrpc":"2.0","id":` + id + `}`)
		}
		return true
	})
}

type session struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan error
}

type env struct {
	cache  string
	starts string
	seen   string
}

func newEnv(t *testing.T) env {
	dir := t.TempDir()
	e := env{cache: filepath.Join(dir, "cache"), starts: filepath.Join(dir, "starts"), seen: filepath.Join(dir, "seen")}
	t.Setenv("LAZYMCP_FAKE_SERVER", "1")
	t.Setenv("LAZYMCP_FAKE_STARTS", e.starts)
	t.Setenv("LAZYMCP_FAKE_SEEN", e.seen)
	return e
}

func (e env) startCount() int {
	b, _ := os.ReadFile(e.starts)
	return strings.Count(string(b), "start")
}

func (e env) cached(key string) (json.RawMessage, bool) {
	return OpenCache(e.cache, fakeCommand).Get(key)
}

func (e env) open(t *testing.T) *session {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{
		Command:       fakeCommand,
		Cache:         OpenCache(e.cache, fakeCommand),
		In:            inR,
		Out:           outW,
		Stderr:        io.Discard,
		ShutdownGrace: 2 * time.Second,
	}
	s := &session{t: t, in: inW, out: bufio.NewReaderSize(outR, 1<<20), done: make(chan error, 1)}
	go func() { s.done <- p.Run(ctx); outW.Close() }()
	t.Cleanup(func() { cancel(); inW.Close() })
	return s
}

func (s *session) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatalf("send: %v", err)
	}
}

func (s *session) recv() string {
	s.t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() { l, err := s.out.ReadString('\n'); ch <- res{strings.TrimRight(l, "\n"), err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			s.t.Fatalf("recv: %v", r.err)
		}
		return r.line
	case <-time.After(10 * time.Second):
		s.t.Fatal("recv: timed out")
	}
	return ""
}

// until reads lines until one carries the given id, answering the server's roots/list
// requests on the way, as a client would. It returns the reply and the other lines.
func (s *session) until(id string) (reply string, others []string) {
	s.t.Helper()
	for {
		got := s.recv()
		switch {
		case string(field(s.t, got, "method")) == `"roots/list"`:
			s.send(rootsReply)
		case string(field(s.t, got, "id")) == id:
			return got, others
		default:
			others = append(others, got)
		}
	}
}

func (s *session) close() error {
	s.in.Close()
	select {
	case err := <-s.done:
		return err
	case <-time.After(10 * time.Second):
		s.t.Fatal("proxy did not exit after stdin closed")
	}
	return nil
}

// The exact opening sequence Claude Code sent, ids included.
const (
	discoverReq = `{"jsonrpc":"2.0","id":"server-discover-probe-1","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	initReq     = `{"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"claude-code","version":"2.1.287"}},"jsonrpc":"2.0","id":0}`
	initedNote  = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	listReq     = `{"method":"tools/list","jsonrpc":"2.0","id":1}`
	rootsReply  = `{"result":{"roots":[{"uri":"file:///work"}]},"jsonrpc":"2.0","id":0}`
)

func field(t *testing.T, line, name string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %q", line)
	}
	return m[name]
}

func opening(t *testing.T, s *session) {
	t.Helper()
	s.send(discoverReq)
	if got := s.recv(); !strings.Contains(got, `-32601`) || string(field(t, got, "id")) != `"server-discover-probe-1"` {
		t.Fatalf("discover: %s", got)
	}
	s.send(initReq)
	if got := s.recv(); string(field(t, got, "id")) != `0` || !strings.Contains(got, `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("initialize: %s", got)
	}
	s.send(initedNote)
}

func TestFirstSessionPassesThroughAndRecords(t *testing.T) {
	e := newEnv(t)
	s := e.open(t)
	opening(t, s)
	// With a cold cache the server is live, so its roots/list reaches the client.
	if got := s.recv(); string(field(t, got, "method")) != `"roots/list"` {
		t.Fatalf("expected roots/list from the server, got %s", got)
	}
	s.send(rootsReply)
	s.send(listReq)
	if got := s.recv(); string(field(t, got, "id")) != `1` || !strings.Contains(got, "evaluate_script") {
		t.Fatalf("tools/list: %s", got)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if n := e.startCount(); n != 1 {
		t.Fatalf("server started %d times, want 1", n)
	}
	for _, k := range []string{"server/discover@2026-07-28", "initialize@2025-11-25", "tools/list@2025-11-25"} {
		if _, ok := e.cached(k); !ok {
			t.Errorf("cache is missing %s", k)
		}
	}
}

func TestWarmSessionNeverStartsAnUnusedServer(t *testing.T) {
	e := newEnv(t)
	prime(t, e)

	s := e.open(t)
	opening(t, s)
	s.send(listReq)
	if got := s.recv(); string(field(t, got, "id")) != `1` || !strings.Contains(got, "evaluate_script") {
		t.Fatalf("tools/list from cache: %s", got)
	}
	s.send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if got := s.recv(); got != `{"jsonrpc":"2.0","id":7,"result":{}}` {
		t.Fatalf("ping: %s", got)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if n := e.startCount(); n != 0 {
		t.Fatalf("server started %d times in a session that never called a tool", n)
	}
}

// The payload is the one sent through evaluate_script in the capture: quotes,
// backslashes, escapes, Hangul and shell metacharacters must survive byte for byte.
func TestToolCallStartsServerAndRelaysVerbatim(t *testing.T) {
	e := newEnv(t)
	prime(t, e)

	s := e.open(t)
	opening(t, s)
	s.send(listReq)
	s.recv()

	args := `{"function":"() => \"quote\\\" back\\\\ nl\\n tab\\t 한글 $(id) ` + "`x`" + ` ${y} <b>&amp;\""}`
	call := `{"method":"tools/call","params":{"name":"evaluate_script","arguments":` + args + `,"_meta":{"progressToken":2}},"jsonrpc":"2.0","id":2}`
	s.send(call)

	// The replayed handshake makes the server ask roots/list; until answers it.
	reply, others := s.until(`2`)
	if len(others) > 0 {
		t.Fatalf("unexpected lines reached the client (proxy-internal responses leaking?): %q", others)
	}
	var r struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(reply), &r); err != nil || len(r.Result.Content) != 1 {
		t.Fatalf("tools/call reply: %s", reply)
	}
	if r.Result.Content[0].Text != args {
		t.Fatalf("arguments were altered\n got: %s\nwant: %s", r.Result.Content[0].Text, args)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if n := e.startCount(); n != 1 {
		t.Fatalf("server started %d times, want 1", n)
	}

	// The server must have seen the client's own initialize params, then the call line verbatim.
	seen, _ := os.ReadFile(e.seen)
	lines := strings.Split(strings.TrimSpace(string(seen)), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], `"lazymcp-init"`) || !strings.Contains(lines[0], `"clientInfo":{"name":"claude-code","version":"2.1.287"}`) {
		t.Fatalf("replayed initialize: %q", lines)
	}
	if !slices.Contains(lines, call) {
		t.Fatalf("server never received the tools/call line verbatim; saw %q", lines)
	}
}

func TestStaleToolListIsCorrected(t *testing.T) {
	e := newEnv(t)
	prime(t, e)
	t.Setenv("LAZYMCP_FAKE_TOOLS", `[{"name":"evaluate_script","inputSchema":{"type":"object"}},{"name":"new_tool","inputSchema":{"type":"object"}}]`)

	s := e.open(t)
	opening(t, s)
	s.send(listReq)
	if got := s.recv(); strings.Contains(got, "new_tool") {
		t.Fatalf("expected the stale cached list first, got %s", got)
	}
	s.send(`{"method":"tools/call","params":{"name":"evaluate_script","arguments":{}},"jsonrpc":"2.0","id":2}`)
	_, others := s.until(`2`)
	if !slices.ContainsFunc(others, func(l string) bool {
		return string(field(t, l, "method")) == `"notifications/tools/list_changed"`
	}) {
		// The notification may also follow the reply.
		if got := s.recv(); string(field(t, got, "method")) != `"notifications/tools/list_changed"` {
			t.Fatalf("no list_changed notification; got %q then %s", others, got)
		}
	}
	s.close()
	if v, _ := e.cached("tools/list@2025-11-25"); !strings.Contains(string(v), "new_tool") {
		t.Fatalf("cache was not updated: %s", v)
	}
}

func TestLargeLinesPassThrough(t *testing.T) {
	e := newEnv(t)
	prime(t, e)
	s := e.open(t)
	opening(t, s)
	big := strings.Repeat("A", 300<<10) // bigger than any bufio default
	s.send(`{"method":"tools/call","params":{"name":"evaluate_script","arguments":{"data":"` + big + `"}},"jsonrpc":"2.0","id":3}`)
	if reply, _ := s.until(`3`); !strings.Contains(reply, big) {
		t.Fatalf("large reply truncated (%d bytes)", len(reply))
	}
	s.close()
}

func TestServerExitEndsSession(t *testing.T) {
	command := []string{"sh", "-c", "exit 3"}
	inR, inW := io.Pipe()
	defer inW.Close()
	p := &Proxy{Command: command, Cache: OpenCache("", command), In: inR, Out: io.Discard, Stderr: io.Discard}
	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background()) }()
	io.WriteString(inW, initReq+"\n")
	select {
	case err := <-done:
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 3 {
			t.Fatalf("got %v, want exit status 3", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("proxy outlived its server")
	}
}

// prime runs one cold session so the cache holds what the server answered.
func prime(t *testing.T, e env) {
	t.Helper()
	s := e.open(t)
	opening(t, s)
	s.recv() // roots/list
	s.send(rootsReply)
	s.send(listReq)
	s.recv()
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	os.Remove(e.starts)
	os.Remove(e.seen)
}
