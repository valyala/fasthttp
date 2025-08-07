package stackless

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewFuncSimple(t *testing.T) {
	t.Parallel()

	var n atomic.Uint64
	f := NewFunc(func(ctx any) {
		n.Add(uint64(ctx.(int)))
	})

	iterations := 4 * 1024
	for i := 0; i < iterations; i++ {
		if !f(2) {
			t.Fatalf("f mustn't return false")
		}
	}
	if got := n.Load(); got != uint64(2*iterations) {
		t.Fatalf("Unexpected n: %d. Expecting %d", got, 2*iterations)
	}
}

func TestNewFuncMulti(t *testing.T) {
	t.Parallel()

	var n1, n2 atomic.Uint64
	f1 := NewFunc(func(ctx any) {
		n1.Add(uint64(ctx.(int)))
	})
	f2 := NewFunc(func(ctx any) {
		n2.Add(uint64(ctx.(int)))
	})

	iterations := 4 * 1024

	f1Done := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < iterations; i++ {
			if !f1(3) {
				err = errors.New("f1 mustn't return false")
				break
			}
		}
		f1Done <- err
	}()

	f2Done := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < iterations; i++ {
			if !f2(5) {
				err = errors.New("f2 mustn't return false")
				break
			}
		}
		f2Done <- err
	}()

	select {
	case err := <-f1Done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}

	select {
	case err := <-f2Done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}

	if got1 := n1.Load(); got1 != uint64(3*iterations) {
		t.Fatalf("unexpected n1: %d. Expecting %d", got1, 3*iterations)
	}
	if got2 := n2.Load(); got2 != uint64(5*iterations) {
		t.Fatalf("unexpected n2: %d. Expecting %d", got2, 5*iterations)
	}
}
