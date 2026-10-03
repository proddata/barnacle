package pgws

import (
	"context"
	"encoding/binary"
	"sync"
	"time"
)

// backendKeyCapture skips message bodies without retaining result rows. It only
// keeps PostgreSQL's small BackendKeyData message for disconnect cancellation.
type backendKeyCapture struct {
	mu        sync.Mutex
	pid       uint32
	secret    []byte
	header    [5]byte
	headerN   int
	remaining uint64
	body      [260]byte
	bodyN     int
	capture   bool
	done      bool
}

func (p *backendKeyCapture) feed(data []byte) {
	for len(data) > 0 && !p.done {
		if p.headerN < len(p.header) {
			n := copy(p.header[p.headerN:], data)
			p.headerN += n
			data = data[n:]
			if p.headerN < len(p.header) {
				return
			}
			length := binary.BigEndian.Uint32(p.header[1:])
			if length < 4 {
				p.done = true
				return
			}
			p.remaining = uint64(length - 4)
			p.capture = p.header[0] == 'K' && p.remaining >= 8 && p.remaining <= uint64(len(p.body))
			p.bodyN = 0
		}
		n := int(min(uint64(len(data)), p.remaining))
		if p.capture {
			copy(p.body[p.bodyN:], data[:n])
			p.bodyN += n
		}
		data = data[n:]
		p.remaining -= uint64(n)
		if p.remaining == 0 {
			if p.capture {
				p.mu.Lock()
				p.pid = binary.BigEndian.Uint32(p.body[:4])
				p.secret = append([]byte(nil), p.body[4:p.bodyN]...)
				p.mu.Unlock()
				p.done = true
			}
			p.headerN = 0
		}
	}
}

func (p *backendKeyCapture) key() (uint32, []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid, append([]byte(nil), p.secret...)
}

func (c Handler) cancelPostgres(addr string, pid uint32, secret []byte) {
	if len(secret) == 0 {
		return
	}
	backend, err := c.DialPostgres(addr)
	if err != nil {
		return
	}
	defer backend.Close()
	_ = backend.SetWriteDeadline(time.Now().Add(5 * time.Second))
	packet := make([]byte, 12+len(secret))
	binary.BigEndian.PutUint32(packet[:4], uint32(len(packet)))
	binary.BigEndian.PutUint32(packet[4:8], 80877102)
	binary.BigEndian.PutUint32(packet[8:12], pid)
	copy(packet[12:], secret)
	_ = writeFull(backend, packet)
}

func (c Handler) cancelDisconnectedSession(addr string, key *backendKeyCapture) {
	pid, secret := key.key()
	if len(secret) == 0 {
		return
	}
	if c.CancelSlots != nil {
		select {
		case c.CancelSlots <- struct{}{}:
		default:
			return
		}
	}
	go func() {
		if c.CancelSlots != nil {
			defer func() { <-c.CancelSlots }()
		}
		c.cancelPostgres(addr, pid, secret)
	}()
}

func (c Handler) cancelForShutdown(ctx context.Context, addr string, key *backendKeyCapture) {
	pid, secret := key.key()
	if len(secret) == 0 {
		return
	}
	if c.CancelSlots != nil {
		select {
		case c.CancelSlots <- struct{}{}:
			defer func() { <-c.CancelSlots }()
		case <-ctx.Done():
			return
		}
	}
	c.cancelPostgres(addr, pid, secret)
}
