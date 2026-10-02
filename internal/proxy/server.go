package proxy

import (
	"encoding/json"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// handler decides what happens to a server response the proxy is waiting for.
// It returns true if the line should still be forwarded to the client.
type handler func(m *message) (forward bool)

// server is the real MCP server once it has been started.
type server struct {
	cmd   *exec.Cmd
	stdin *lineWriter

	mu      sync.Mutex
	pending map[string]handler

	exited chan struct{} // closed once stdout hit EOF and the process was reaped
}

func startServer(command []string, stderr io.Writer, out *lineWriter) (*server, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = stderr
	cmd.SysProcAttr = sysProcAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &server{cmd: cmd, stdin: &lineWriter{w: stdin}, pending: map[string]handler{}, exited: make(chan struct{})}
	go func() {
		// Relay server output line by line; dispatch only parses while a response
		// is pending.
		eachLine(stdout, func(b []byte) bool {
			if s.dispatch(b) {
				out.writeLine(b)
			}
			return true
		})
		cmd.Wait()
		close(s.exited)
	}()
	return s, nil
}

func (s *server) dispatch(b []byte) bool {
	s.mu.Lock()
	idle := len(s.pending) == 0
	s.mu.Unlock()
	if idle {
		return true
	}
	var m message
	if json.Unmarshal(b, &m) != nil || !m.isResponse() {
		return true
	}
	key := canonical(m.ID)
	s.mu.Lock()
	h, ok := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if !ok {
		return true
	}
	return h(&m)
}

// expect registers h for the response carrying id. It must be called before the
// request is written, or a fast server could answer first.
func (s *server) expect(id json.RawMessage, h handler) {
	s.mu.Lock()
	s.pending[canonical(id)] = h
	s.mu.Unlock()
}

func (s *server) write(b []byte) error { return s.stdin.writeLine(b) }

func (s *server) send(v any) error {
	b, err := encode(v)
	if err != nil {
		return err
	}
	return s.write(b)
}

// stop ends the server and everything it spawned. The server runs in its own process
// group because npx-style launchers leave helpers behind; signalling only the direct
// child would orphan them, which is the very thing this proxy exists to prevent.
// Safe to call on a server that already exited.
func (s *server) stop(grace time.Duration) {
	s.stdin.close()
	if !s.waitFor(grace) {
		killGroup(s.cmd.Process.Pid, syscall.SIGTERM)
		if !s.waitFor(2 * time.Second) {
			killGroup(s.cmd.Process.Pid, syscall.SIGKILL)
			<-s.exited
		}
	}
	// The leader may exit cleanly while helpers in its group live on.
	killGroup(s.cmd.Process.Pid, syscall.SIGTERM)
}

func (s *server) waitFor(d time.Duration) bool {
	select {
	case <-s.exited:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *server) exitCode() int {
	if s.cmd.ProcessState == nil {
		return 1
	}
	if code := s.cmd.ProcessState.ExitCode(); code >= 0 {
		return code
	}
	return 1
}
