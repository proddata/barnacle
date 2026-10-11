package pgws

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/proddata/barnacle/internal/gateway"
)

func TestBackendKeyCaptureSkipsRowsAndHandlesSplitMessages(t *testing.T) {
	parser := &backendKeyCapture{}
	row := bytes.Repeat([]byte{'K'}, 1<<20)
	rowHeader := []byte{'D', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(rowHeader[1:], uint32(len(row)+4))
	key := []byte{'K', 0, 0, 0, 12, 0, 0, 0, 42, 1, 2, 3, 4}
	for _, chunk := range [][]byte{
		rowHeader[:2], rowHeader[2:], row[:500], row[500:], key[:3], key[3:8], key[8:],
	} {
		parser.feed(chunk)
	}
	pid, secret := parser.key()
	if pid != 42 || !bytes.Equal(secret, []byte{1, 2, 3, 4}) {
		t.Fatalf("backend key = %d, %v", pid, secret)
	}
	parser.feed([]byte{'K', 0, 0, 0, 12, 0, 0, 0, 99, 9, 9, 9, 9})
	pid, _ = parser.key()
	if pid != 42 {
		t.Fatal("backend key changed after capture")
	}
}

func TestShutdownCancellationWaitsOnlyUntilDeadline(t *testing.T) {
	key := &backendKeyCapture{}
	key.feed([]byte{'K', 0, 0, 0, 12, 0, 0, 0, 42, 1, 2, 3, 4})
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	handler := New(&gateway.Config{CancelSlots: slots})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	handler.cancelForShutdown(ctx, "127.0.0.1:1", key)
	if time.Since(started) > time.Second {
		t.Fatal("shutdown cancellation exceeded its deadline")
	}
	if len(slots) != 1 {
		t.Fatal("shutdown cancellation disturbed a busy slot")
	}
}

func TestDisconnectedCancellationWaitsForSlot(t *testing.T) {
	key := &backendKeyCapture{}
	key.feed([]byte{'K', 0, 0, 0, 12, 0, 0, 0, 42, 1, 2, 3, 4})
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	handler := New(&gateway.Config{CancelSlots: slots})
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		handler.cancelDisconnectedSession("127.0.0.1:1", key)
		close(done)
	}()
	<-started
	select {
	case <-done:
		t.Fatal("busy cancellation slot caused request to be dropped")
	case <-time.After(50 * time.Millisecond):
	}
	<-slots
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not proceed after a slot became available")
	}
}
