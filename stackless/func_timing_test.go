package stackless

import (
	"sync/atomic"
	"testing"
)

func BenchmarkFuncOverhead(b *testing.B) {
	var n atomic.Uint64
	f := NewFunc(func(ctx any) {
		n.Add(*(ctx.(*uint64)))
	})
	b.RunParallel(func(pb *testing.PB) {
		x := uint64(1)
		for pb.Next() {
			if !f(&x) {
				b.Fatalf("f mustn't return false")
			}
		}
	})
	if got := n.Load(); got != uint64(b.N) {
		b.Fatalf("unexpected n: %d. Expecting %d", got, b.N)
	}
}

func BenchmarkFuncPure(b *testing.B) {
	var n atomic.Uint64
	f := func(x *uint64) {
		n.Add(*x)
	}
	b.RunParallel(func(pb *testing.PB) {
		x := uint64(1)
		for pb.Next() {
			f(&x)
		}
	})
	if got := n.Load(); got != uint64(b.N) {
		b.Fatalf("unexpected n: %d. Expecting %d", got, b.N)
	}
}
