package gateway

import (
	"math"
	"sync"
	"testing"
)

func TestMemoryBudgetReservesAndReleases(t *testing.T) {
	b := NewMemoryBudget(100)
	if !b.TryReserve(60) || b.TryReserve(41) || !b.TryReserve(40) {
		t.Fatal("reservations did not follow the budget")
	}
	b.Release(60)
	if !b.TryReserve(60) {
		t.Fatal("released memory was not available again")
	}
	var nilBudget *MemoryBudget
	if nilBudget.Max() != 0 {
		t.Fatal("nil budget reports a limit")
	}
}

func TestMemoryBudgetNeverOvercommitsUnderConcurrency(t *testing.T) {
	const limit = 1000
	b := NewMemoryBudget(limit)
	var mu sync.Mutex
	var held, peak int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				if !b.TryReserve(37) {
					continue
				}
				mu.Lock()
				held += 37
				peak = max(peak, held)
				mu.Unlock()
				mu.Lock()
				held -= 37
				mu.Unlock()
				b.Release(37)
			}
		}()
	}
	wg.Wait()
	if peak > limit {
		t.Fatalf("peak reservation %d exceeded the %d budget", peak, limit)
	}
	if !b.TryReserve(limit) {
		t.Fatal("budget did not return to empty")
	}
}

func TestMemoryBudgetFor(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		name   string
		goMem  int64
		want   int64
		source string
	}{
		{"packaged unit", 160 * mib, 96 * mib, "GOMEMLIMIT"},
		{"limit raised with GOMEMLIMIT", 320 * mib, 256 * mib, "GOMEMLIMIT"},
		{"no GOMEMLIMIT", math.MaxInt64, 96 * mib, "default"},
		{"tiny limit keeps a usable floor", 70 * mib, 16 * mib, "GOMEMLIMIT"},
	} {
		got, source := memoryBudgetFor(tc.goMem, 96*mib)
		if got != tc.want || source != tc.source {
			t.Errorf("%s: budget %d MiB from %q, want %d MiB from %q", tc.name, got/mib, source, tc.want/mib, tc.source)
		}
	}
}
