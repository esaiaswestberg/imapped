package fakeimap

import (
	"net"
	"strings"
	"sync"
)

// Recorder captures every command line a client sends.
//
// This is what makes the central performance property testable. The Rust
// implementation issued one FETCH per message per sync — roughly 16,600
// commands for 8,300 messages, every run, forever. Asserting on a command count
// catches a regression to that shape immediately, in a unit test, rather than
// two days into a production sync.
type Recorder struct {
	mu       sync.Mutex
	commands []string
}

func newRecorder() *Recorder { return &Recorder{} }

func (r *Recorder) record(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, line)
}

// Commands returns every recorded command line, including its tag.
func (r *Recorder) Commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.commands...)
}

// Count returns how many commands contain substr, matched case-insensitively.
//
// The IMAP tag prefixes each line, so callers should match on the command verb
// rather than anchoring to the start of the line.
func (r *Recorder) Count(substr string) int {
	substr = strings.ToUpper(substr)
	n := 0
	for _, cmd := range r.Commands() {
		if strings.Contains(strings.ToUpper(cmd), substr) {
			n++
		}
	}
	return n
}

// Reset clears the log, so a test can measure one phase in isolation.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = nil
}

// String renders the log for test failure messages.
func (r *Recorder) String() string {
	return strings.Join(r.Commands(), "\n")
}

// recordingConn splits the client's byte stream into lines and records them.
//
// IMAP literals mean not every line is a command — an APPEND's payload arrives
// as raw octets — but for the assertions this harness supports (counting
// FETCH, SELECT and so on) treating lines as commands is accurate enough, and
// far simpler than reimplementing the protocol parser.
type recordingConn struct {
	net.Conn
	recorder *Recorder
	pending  []byte
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.pending = append(c.pending, p[:n]...)
		for {
			idx := indexByte(c.pending, '\n')
			if idx < 0 {
				break
			}
			c.recorder.record(string(c.pending[:idx]))
			c.pending = c.pending[idx+1:]
		}
		// Bound the buffer so a large literal cannot grow it without limit.
		if len(c.pending) > 64*1024 {
			c.pending = c.pending[:0]
		}
	}
	return n, err
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
