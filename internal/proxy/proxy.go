// Package proxy starts a stdio MCP server only when a client first needs it.
//
// Until then the proxy answers the session-setup requests (server/discover,
// initialize, tools/list, ...) from answers it recorded from the same server
// earlier. The first request it cannot answer — normally the first tools/call —
// starts the server, replays the client's handshake to it, and from then on the
// proxy relays bytes in both directions.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"runtime"
	"sync"
	"time"
)

// Ids of the requests the proxy sends on its own. They are strings, while clients in
// practice number their requests, so the two can never collide.
var (
	initID     = json.RawMessage(`"lazymcp-init"`)
	logLevelID = json.RawMessage(`"lazymcp-loglevel"`)
)

// listChanged maps a cacheable list method to the notification that tells the client
// to fetch it again.
var listChanged = map[string]string{
	"tools/list":               "notifications/tools/list_changed",
	"prompts/list":             "notifications/prompts/list_changed",
	"resources/list":           "notifications/resources/list_changed",
	"resources/templates/list": "notifications/resources/list_changed",
}

// ExitError reports that the wrapped server exited on its own.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("server exited with status %d", e.Code) }

type Proxy struct {
	Command []string
	Cache   *Cache
	In      io.Reader
	Out     io.Writer
	Stderr  io.Writer // the server's stderr
	Log     *log.Logger
	Verbose bool

	// InitTimeout bounds the replayed handshake once the server is started.
	InitTimeout time.Duration
	// ShutdownGrace is how long the server gets to exit after its stdin closes.
	ShutdownGrace time.Duration

	out *lineWriter
	srv *server

	// Recorded from the client before the server exists, replayed when it starts.
	initParams     json.RawMessage
	sawInitialized bool
	logLevel       json.RawMessage
	served         map[string]bool // list methods answered from the cache

	mu         sync.Mutex
	negotiated string // protocolVersion of this session
}

// Run serves one client session. It returns nil when the client hangs up, an
// *ExitError if the server exits first, or the context's error.
func (p *Proxy) Run(ctx context.Context) error {
	// See sysProcAttr: Pdeathsig is tied to the forking thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if p.InitTimeout == 0 {
		p.InitTimeout = 60 * time.Second
	}
	if p.ShutdownGrace == 0 {
		p.ShutdownGrace = 5 * time.Second
	}
	if p.Log == nil {
		p.Log = log.New(io.Discard, "", 0)
	}
	p.out = &lineWriter{w: p.Out}
	p.served = map[string]bool{}

	lines := make(chan []byte)
	go func() {
		defer close(lines)
		eachLine(p.In, func(b []byte) bool {
			select {
			case lines <- b:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()

	for {
		var exited <-chan struct{}
		if p.srv != nil {
			exited = p.srv.exited
		}
		select {
		case b, ok := <-lines:
			if !ok {
				p.stopServer()
				return nil
			}
			if err := p.handleClient(b); err != nil {
				p.stopServer()
				return err
			}
		case <-exited:
			p.stopServer()
			return &ExitError{Code: p.srv.exitCode()}
		case <-ctx.Done():
			p.stopServer()
			return ctx.Err()
		}
	}
}

func (p *Proxy) handleClient(b []byte) error {
	if p.srv != nil {
		p.watch(b)
		return p.srv.write(b)
	}

	var m message
	if err := json.Unmarshal(b, &m); err != nil {
		return p.reply(json.RawMessage("null"), response{Error: json.RawMessage(`{"code":-32700,"message":"Parse error"}`)})
	}
	switch {
	case m.isResponse():
		// Nothing has asked the client anything yet.
		p.debugf("dropping response %s received before the server started", m.ID)
		return nil
	case m.isNotification():
		if m.Method == "notifications/initialized" {
			p.sawInitialized = true
		} else {
			p.debugf("dropping %s received before the server started", m.Method)
		}
		return nil
	case !m.isRequest():
		return p.reply(json.RawMessage("null"), response{Error: json.RawMessage(`{"code":-32600,"message":"Invalid Request"}`)})
	}

	switch m.Method {
	case "ping":
		return p.reply(m.ID, response{Result: json.RawMessage(`{}`)})
	case "logging/setLevel":
		p.logLevel = m.Params
		return p.reply(m.ID, response{Result: json.RawMessage(`{}`)})
	}

	reason := "uncacheable request"
	if key, ok := p.cacheKey(&m); ok {
		if raw, hit := p.Cache.Get(key); hit {
			var resp response
			if json.Unmarshal(raw, &resp) == nil {
				p.observe(&m, resp)
				if listChanged[m.Method] != "" {
					p.served[m.Method] = true
				}
				p.debugf("answered %s from cache", key)
				return p.reply(m.ID, resp)
			}
		}
		reason = "cache miss"
	}

	p.Log.Printf("starting server: %s %s", reason, m.Method)
	if err := p.start(); err != nil {
		return err
	}
	if m.Method != "initialize" && p.initParams != nil {
		if err := p.handshake(); err != nil {
			return err
		}
	}
	p.watch(b)
	return p.srv.write(b)
}

func (p *Proxy) start() error {
	srv, err := startServer(p.Command, p.Stderr, p.out)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	p.srv = srv
	return nil
}

// observe updates the session state an answer implies, whichever way the answer
// arrived: from the cache, passing through, or from the replayed handshake.
func (p *Proxy) observe(req *message, resp response) {
	if req.Method != "initialize" {
		return
	}
	p.initParams = req.Params
	if v := protocolVersionOf(resp.Result); v != "" {
		p.mu.Lock()
		p.negotiated = v
		p.mu.Unlock()
	}
}

// handshake replays to the fresh server what the client already did with the
// proxy: initialize (the client's own params), initialized, and its log level.
// The server's answers are swallowed — the client already has them.
func (p *Proxy) handshake() error {
	req := message{ID: initID, Method: "initialize", Params: p.initParams}
	got := make(chan *message, 1)
	p.srv.expect(initID, func(m *message) bool { got <- m; return false })
	if err := p.srv.send(outRequest{JSONRPC: "2.0", ID: req.ID, Method: req.Method, Params: req.Params}); err != nil {
		return err
	}
	select {
	case m := <-got:
		if m.Error != nil {
			return fmt.Errorf("server rejected the replayed initialize: %s", m.Error)
		}
		prev := p.version()
		p.observe(&req, responseOf(m))
		if v := p.version(); v != prev {
			p.Log.Printf("warning: server negotiated %q but the cached answer said %q", v, prev)
		}
		if key, ok := p.cacheKey(&req); ok {
			p.store(key, responseOf(m))
		}
	case <-p.srv.exited:
		return &ExitError{Code: p.srv.exitCode()}
	case <-time.After(p.InitTimeout):
		return fmt.Errorf("server did not answer initialize within %s", p.InitTimeout)
	}

	if p.sawInitialized {
		if err := p.srv.send(outRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
			return err
		}
	}
	if p.logLevel != nil {
		p.srv.expect(logLevelID, func(*message) bool { return false })
		if err := p.srv.send(outRequest{JSONRPC: "2.0", ID: logLevelID, Method: "logging/setLevel", Params: p.logLevel}); err != nil {
			return err
		}
	}
	for method := range p.served {
		if err := p.refresh(method); err != nil {
			return err
		}
	}
	return nil
}

// refresh re-asks the server a list the client got from the cache. If the answer
// changed (new server version behind the same command line, say), the cache is
// corrected and the client is told to fetch the list again.
func (p *Proxy) refresh(method string) error {
	id, _ := json.Marshal("lazymcp-refresh-" + method)
	req := message{ID: id, Method: method}
	p.record(&req, false, func() {
		p.Log.Printf("%s changed since it was cached; notifying the client", method)
		if b, err := encode(outRequest{JSONRPC: "2.0", Method: listChanged[method]}); err == nil {
			p.out.writeLine(b)
		}
	})
	return p.srv.send(outRequest{JSONRPC: "2.0", ID: id, Method: method})
}

// watch records the server's answer to a cacheable request that passes through, so
// the next session can be answered without starting the server. The line itself is
// forwarded untouched by the caller.
func (p *Proxy) watch(b []byte) {
	var m message
	if json.Unmarshal(b, &m) != nil || !m.isRequest() {
		return
	}
	p.record(&m, true, nil)
}

// record arranges for the server's answer to req to be observed and cached, if req is
// cacheable and the answer replayable. forward says whether the client gets the answer
// (it does for its own requests, not for the proxy's); onChange runs if the cached
// answer differed.
func (p *Proxy) record(req *message, forward bool, onChange func()) {
	key, ok := p.cacheKey(req)
	if !ok {
		return
	}
	p.srv.expect(req.ID, func(m *message) bool {
		if replayable(m) {
			resp := responseOf(m)
			p.observe(req, resp)
			if p.store(key, resp) && onChange != nil {
				onChange()
			}
		}
		return forward
	})
}

// replayable says whether an answer may be given again to a later session. Errors
// usually reflect a moment (a timeout, a bad state), so they are not — except
// "method not found", which is a fixed property of the server. That one matters:
// it is how servers answer Claude Code's server/discover probe, and not caching it
// would start the server in every session.
func replayable(r *message) bool {
	if r.Error == nil {
		return true
	}
	var e struct {
		Code int `json:"code"`
	}
	return json.Unmarshal(r.Error, &e) == nil && e.Code == -32601
}

// cacheKey says whether a request's answer can be replayed, and under which key.
// Answers depend on the protocol version, so it is part of every key.
func (p *Proxy) cacheKey(m *message) (string, bool) {
	key := func(v string) string { return m.Method + "@" + v }
	switch m.Method {
	case "initialize":
		v := protocolVersionOf(m.Params)
		return key(v), v != ""
	case "server/discover":
		var params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		}
		var v string
		if json.Unmarshal(m.Params, &params) == nil {
			_ = json.Unmarshal(params.Meta["io.modelcontextprotocol/protocolVersion"], &v)
		}
		return key(v), true
	}
	if listChanged[m.Method] == "" {
		return "", false
	}
	v := p.version()
	if v == "" {
		return "", false
	}
	// A paginated follow-up depends on a cursor the cache knows nothing about.
	var params struct {
		Cursor *string `json:"cursor"`
	}
	if len(m.Params) > 0 && (json.Unmarshal(m.Params, &params) != nil || params.Cursor != nil) {
		return "", false
	}
	return key(v), true
}

func (p *Proxy) version() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.negotiated
}

// store caches resp under key and reports whether that changed the cached answer.
func (p *Proxy) store(key string, resp response) bool {
	b, err := encode(resp)
	if err != nil {
		p.Log.Printf("cache: %v", err)
		return false
	}
	changed, err := p.Cache.Put(key, b)
	if err != nil {
		p.Log.Printf("cache: %v", err)
	}
	if changed {
		p.debugf("recorded %s", key)
	}
	return changed
}

func (p *Proxy) reply(id json.RawMessage, resp response) error {
	b, err := encode(outResponse{JSONRPC: "2.0", ID: id, response: resp})
	if err != nil {
		return err
	}
	return p.out.writeLine(b)
}

func (p *Proxy) stopServer() {
	if p.srv != nil {
		p.srv.stop(p.ShutdownGrace)
	}
}

func (p *Proxy) debugf(format string, args ...any) {
	if p.Verbose {
		p.Log.Printf(format, args...)
	}
}

// eachLine calls fn with every non-empty line of r, without its line ending, until r
// ends or fn returns false. Lines have no length limit (bufio.Scanner would impose
// one; a screenshot in base64 is a single line of tens of kilobytes).
func eachLine(r io.Reader, fn func([]byte) bool) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		b, err := br.ReadBytes('\n')
		if b = bytes.TrimRight(b, "\r\n"); len(b) > 0 && !fn(b) {
			return
		}
		if err != nil {
			return
		}
	}
}

// lineWriter serialises whole lines onto one stream: the proxy's own replies and
// the server relay share the client's stdout, and the proxy's requests and the
// client relay share the server's stdin.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// writeLine appends the line ending in place. Callers pass lines whose ending was
// just trimmed (or fresh encoder output), so the append normally reuses spare
// capacity rather than copying a large line.
func (l *lineWriter) writeLine(b []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.w.Write(append(b, '\n'))
	return err
}

func (l *lineWriter) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.w.(io.Closer); ok {
		c.Close()
	}
}
