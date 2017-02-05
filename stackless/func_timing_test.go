package stackless

import (
	"sync/atomic"
	"testing"
)

func BenchmarkFuncOverhead(b *testing.B) {
	var n uint64
	f := NewFunc(func(ctx interface{}) {
		atomic.AddUint64(&n, *(ctx.(*uint64)))
	})
	b.RunParallel(func(pb *testing.PB) {
		x := uint64(1)
		for pb.Next() {
			if !f(&x) {
				b.Fatalf("f mustn't return false")
			}
		}
	})
	if n != uint64(b.N) {
		b.Fatalf("unexected n: %d. Expecting %d", n, b.N)
	}
}

func BenchmarkFuncPure(b *testing.B) {
	var n uint64
	f := func(x *uint64) {
		atomic.AddUint64(&n, *x)
	}
	b.RunParallel(func(pb *testing.PB) {
		x := uint64(1)
		for pb.Next() {
			f(&x)
		}
	})
	if n != uint64(b.N) {
		b.Fatalf("unexected n: %d. Expecting %d", n, b.N)
	}
}
