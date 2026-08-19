package fakeimap

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Chaos makes the transport misbehave in ways a correct server never would.
//
// These are not hypothetical failure modes. A server that accepts a command and
// then never answers is exactly what wedged the previous implementation for two
// days at zero CPU, and no well-behaved fake can reproduce it. The zero value
// disables all of it.
type Chaos struct {
	// HangAfter makes the server stop responding once the client has sent this
	// many commands. The connection stays open and the TCP session stays
	// healthy; it simply never produces another byte. Without a read deadline
	// this hangs the client forever, which is the property under test.
	HangAfter int

	// DropAfter closes the connection abruptly after this many commands,
	// modelling a middlebox or server that severs the session mid-response.
	DropAfter int

	// SlowBytes writes responses at approximately this many bytes per second.
	// This separates an inactivity timeout from a total one: a slow transfer
	// making steady progress should survive, a stalled one should not.
	SlowBytes int

	// RefuseAfter stops accepting new connections after this many have been
	// accepted, modelling a per-user connection cap.
	RefuseAfter int
}

func (c Chaos) enabled() bool {
	return c.HangAfter > 0 || c.DropAfter > 0 || c.SlowBytes > 0 || c.RefuseAfter > 0
}

// wrappedListener applies recording and chaos to every accepted connection.
type wrappedListener struct {
	net.Listener
	recorder *Recorder
	chaos    Chaos

	accepted atomic.Int64
}

func (l *wrappedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	n := l.accepted.Add(1)
	if l.chaos.RefuseAfter > 0 && n > int64(l.chaos.RefuseAfter) {
		// Refuse by closing immediately, as a server at its connection limit
		// typically does after emitting a NO.
		_ = conn.Close()
		return l.Accept()
	}

	var wrapped net.Conn = conn
	if l.chaos.enabled() {
		wrapped = newChaosConn(wrapped, l.chaos)
	}
	if l.recorder != nil {
		wrapped = &recordingConn{Conn: wrapped, recorder: l.recorder}
	}
	return wrapped, nil
}

// chaosConn counts commands by counting newlines the client sends, then applies
// the configured misbehaviour once the threshold is crossed.
type chaosConn struct {
	net.Conn
	chaos Chaos

	// Closed by Close, which releases any read parked in the hang state. Without
	// this a hung connection would leak its goroutine for the life of the test
	// binary.
	done      chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	commands int
	hanging  bool
	dropped  bool
}

func newChaosConn(conn net.Conn, chaos Chaos) *chaosConn {
	return &chaosConn{Conn: conn, chaos: chaos, done: make(chan struct{})}
}

func (c *chaosConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return c.Conn.Close()
}

func (c *chaosConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	hanging := c.hanging
	c.mu.Unlock()

	if hanging {
		// Park until the connection is closed. From the client's side this is
		// indistinguishable from a server that has stopped responding while the
		// TCP session remains healthy.
		<-c.done
		return 0, net.ErrClosed
	}

	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		for _, b := range p[:n] {
			if b == '\n' {
				c.commands++
			}
		}
		if c.chaos.HangAfter > 0 && c.commands >= c.chaos.HangAfter {
			c.hanging = true
		}
		if c.chaos.DropAfter > 0 && c.commands >= c.chaos.DropAfter {
			c.dropped = true
		}
		dropped := c.dropped
		c.mu.Unlock()

		if dropped {
			_ = c.Conn.Close()
		}
	}
	return n, err
}

func (c *chaosConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	hanging, dropped := c.hanging, c.dropped
	rate := c.chaos.SlowBytes
	c.mu.Unlock()

	if dropped {
		return 0, net.ErrClosed
	}
	if hanging {
		// Swallow the response: the server has gone silent, not gone away.
		return len(p), nil
	}

	if rate <= 0 {
		return c.Conn.Write(p)
	}

	// Trickle the response out in small chunks, pacing to the configured rate.
	const chunk = 64
	written := 0
	for written < len(p) {
		end := min(written+chunk, len(p))
		n, err := c.Conn.Write(p[written:end])
		written += n
		if err != nil {
			return written, err
		}
		time.Sleep(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
	}
	return written, nil
}
