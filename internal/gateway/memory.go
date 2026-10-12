package gateway

import (
	"math"
	"runtime/debug"
	"sync"
)

// memoryReserve is what GOMEMLIMIT keeps for everything but buffered request and
// result memory: the Go runtime, garbage waiting for collection, the 32
// WebSocket relays, and TLS. It is a fixed amount, not a share, because in
// measurements the peak was the budget plus roughly 50 to 85 MiB at both 256 and
// 512 MiB, so larger services can give more of their limit to the budget. The
// budget counts estimated live heap and the estimate errs high. With the
// packaged GOMEMLIMIT of 160 MiB the budget is 96 MiB. See
// docs/decisions/memory-limits.md before changing it.
const (
	memoryReserve   = 64 << 20
	minMemoryBudget = 16 << 20
)

// MemoryBudget bounds the estimated heap that buffered HTTP request bodies and
// results may hold at once, across every request. The per-request limits alone
// cannot do this: they cap one request, not how many large ones run
// concurrently, and they count wire bytes while decoded values occupy several
// times that.
//
// Reservations never wait. A request that does not fit fails and returns what
// it holds, so no two requests can hold memory while waiting on each other.
type MemoryBudget struct {
	mu   sync.Mutex
	used int64
	max  int64
}

func NewMemoryBudget(max int64) *MemoryBudget { return &MemoryBudget{max: max} }

// Max returns the total budget; a nil budget is unlimited and reports zero.
func (b *MemoryBudget) Max() int64 {
	if b == nil {
		return 0
	}
	return b.max
}

// TryReserve takes n bytes if they are free.
func (b *MemoryBudget) TryReserve(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.max-b.used {
		return false
	}
	b.used += n
	return true
}

func (b *MemoryBudget) Release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
}

// DefaultMemoryBudget sizes the budget as GOMEMLIMIT less a fixed reserve, so that raising the
// memory limit together with GOMEMLIMIT raises the budget without a third
// setting. It returns the fallback when GOMEMLIMIT is unset.
func DefaultMemoryBudget(fallback int64) (budget int64, source string) {
	return memoryBudgetFor(debug.SetMemoryLimit(-1), fallback)
}

func memoryBudgetFor(goLimit, fallback int64) (int64, string) {
	if goLimit <= 0 || goLimit == math.MaxInt64 {
		return fallback, "default"
	}
	return max(goLimit-memoryReserve, minMemoryBudget), "GOMEMLIMIT"
}
