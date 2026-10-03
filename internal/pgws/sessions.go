package pgws

import (
	"context"
	"net"
	"sync"
	"time"
)

type wsSession struct {
	client, backend net.Conn
	writer          *wsWriter
	cancel          func(context.Context)
}

func (s *wsSession) closeForShutdown(ctx context.Context) {
	s.writer.closing.Store(true)
	_ = s.client.SetWriteDeadline(time.Now().Add(time.Second))
	_ = s.writer.frame(8, []byte{0x03, 0xe9}) // 1001: going away
	if s.cancel != nil {
		s.cancel(ctx)
	}
	_ = s.client.Close()
	_ = s.backend.Close()
}

type sessionRegistry struct {
	mu       sync.Mutex
	active   map[*wsSession]struct{}
	draining bool
	done     chan struct{}
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{active: make(map[*wsSession]struct{}), done: make(chan struct{})}
}

func (r *sessionRegistry) isDraining() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draining
}

func (r *sessionRegistry) add(session *wsSession) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return false
	}
	r.active[session] = struct{}{}
	return true
}

func (r *sessionRegistry) remove(session *wsSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.active, session)
	if r.draining && len(r.active) == 0 {
		close(r.done)
	}
}

func (r *sessionRegistry) shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.draining {
		done := r.done
		r.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.draining = true
	sessions := make([]*wsSession, 0, len(r.active))
	for session := range r.active {
		sessions = append(sessions, session)
	}
	if len(sessions) == 0 {
		close(r.done)
	}
	done := r.done
	r.mu.Unlock()
	var closes sync.WaitGroup
	for _, session := range sessions {
		closes.Add(1)
		go func() {
			defer closes.Done()
			session.closeForShutdown(ctx)
		}()
	}
	finished := make(chan struct{})
	go func() {
		<-done
		closes.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		for _, session := range sessions {
			_ = session.client.Close()
			_ = session.backend.Close()
		}
		return ctx.Err()
	}
}

// Keep the close deadline short even when an ordinary frame has a longer limit.
func (w *wsWriter) shutdownWriteTimeout() time.Duration {
	if w.closing.Load() {
		return time.Second
	}
	return w.writeTimeout
}
