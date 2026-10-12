package sqlhttp

import (
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"time"
)

// livePeak runs fn and returns the highest live heap it reached above what was
// live before it started, sampling with the collector running normally. Total
// allocation would also count garbage that is freed long before the peak.
func livePeak(fn func()) int64 {
	runtime.GC()
	read := func() uint64 {
		sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	base := read()
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if v := read(); v > peak.Load() {
				peak.Store(v)
			}
			time.Sleep(20 * time.Microsecond)
		}
	}()
	fn()
	if v := read(); v > peak.Load() {
		peak.Store(v)
	}
	close(stop)
	<-done
	return int64(peak.Load()) - int64(base)
}
