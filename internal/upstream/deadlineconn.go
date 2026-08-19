package upstream

import (
	"net"
	"sync"
	"time"
)

// deadlineConn applies an inactivity timeout to a connection by resetting the
// read deadline on every successful read and the write deadline on every write.
//
// This is deliberately an *idle* timeout rather than a total one. A 200MB
// message body may legitimately take minutes to transfer, and a total deadline
// would abort it; but a connection that has produced no bytes at all for a
// minute is dead regardless of how long the operation is expected to take.
// The Rust implementation had neither, which is how a silently-dropped TCP
// connection wedged a sync for two days at zero CPU.
type deadlineConn struct {
	net.Conn

	idle time.Duration

	// Guards deadline updates. Read and Write can run concurrently (go-imap
	// writes commands while a reader goroutine consumes responses), and
	// SetDeadline is not documented as safe for concurrent use.
	mu sync.Mutex
}

func newDeadlineConn(conn net.Conn, idle time.Duration) net.Conn {
	if idle <= 0 {
		return conn
	}
	dc := &deadlineConn{Conn: conn, idle: idle}
	dc.touchRead()
	dc.touchWrite()
	return dc
}

func (c *deadlineConn) touchRead() {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
}

func (c *deadlineConn) touchWrite() {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		// Progress was made, so extend the window.
		c.touchRead()
	}
	return n, err
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	c.touchWrite()
	return c.Conn.Write(p)
}
