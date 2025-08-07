package fasthttp

import (
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkCoarseTimeNow(b *testing.B) {
	var zeroTimeCount atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			t := CoarseTimeNow()
			if t.IsZero() {
				zeroTimeCount.Add(1)
			}
		}
	})
	if zeroTimeCount.Load() > 0 {
		b.Fatalf("zeroTimeCount must be zero")
	}
}

func BenchmarkTimeNow(b *testing.B) {
	var zeroTimeCount atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			t := time.Now()
			if t.IsZero() {
				zeroTimeCount.Add(1)
			}
		}
	})
	if zeroTimeCount.Load() > 0 {
		b.Fatalf("zeroTimeCount must be zero")
	}
}
