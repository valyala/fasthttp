package fasthttp

import (
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkCoarseTimeNow(b *testing.B) {
	var zeroTimeCount uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			t := CoarseTimeNow()
			if t.IsZero() {
				atomic.AddUint64(&zeroTimeCount, 1)
			}
		}
	})
	if zeroTimeCount > 0 {
		b.Fatalf("zeroTimeCount must be zero")
	}
}

func BenchmarkTimeNow(b *testing.B) {
	var zeroTimeCount uint64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			t := time.Now()
			if t.IsZero() {
				atomic.AddUint64(&zeroTimeCount, 1)
			}
		}
	})
	if zeroTimeCount > 0 {
		b.Fatalf("zeroTimeCount must be zero")
	}
}
