package gateway

import (
	"net"
	"sync"
)

// LimitListener bounds accepted connections for their entire lifetime,
// including TLS handshakes, HTTP keep-alive, and hijacked WebSockets. At
// capacity it pauses Accept; additional clients wait in the OS listen backlog.
// Closing the listener wakes Accept even while all slots are occupied.
func LimitListener(listener net.Listener, limit int) net.Listener {
	if limit <= 0 {
		panic("listener connection limit must be positive")
	}
	return &limitedListener{
		Listener: listener,
		slots:    make(chan struct{}, limit),
		done:     make(chan struct{}),
	}
}

type limitedListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitedConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitedConn struct {
	net.Conn
	release     func()
	releaseOnce sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}
