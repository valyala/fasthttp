// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package atomic

import (
	"runtime"
	"sync"
	"testing"
)

const (
	_parallelism = 4
	_iterations  = 1000
)

func runStress(f func()) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(_parallelism))

	var wg sync.WaitGroup
	wg.Add(_parallelism)
	for i := 0; i < _parallelism; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < _iterations; j++ {
				f()
			}
		}()
	}

	wg.Wait()
}

func TestStressInt32(t *testing.T) {
	var atom Int32
	runStress(func() {
		atom.Load()
		atom.Add(1)
		atom.Sub(2)
		atom.Inc()
		atom.Dec()
		atom.CAS(1, 0)
		atom.Swap(5)
		atom.Store(1)
	})
}

func TestStressInt64(t *testing.T) {
	var atom Int64
	runStress(func() {
		atom.Load()
		atom.Add(1)
		atom.Sub(2)
		atom.Inc()
		atom.Dec()
		atom.CAS(1, 0)
		atom.Swap(5)
		atom.Store(1)

	})
}

func TestStressUint32(t *testing.T) {
	var atom Uint32
	runStress(func() {
		atom.Load()
		atom.Add(1)
		atom.Sub(2)
		atom.Inc()
		atom.Dec()
		atom.CAS(1, 0)
		atom.Swap(5)
		atom.Store(1)
	})
}

func TestStressUint64(t *testing.T) {
	var atom Uint64
	runStress(func() {
		atom.Load()
		atom.Add(1)
		atom.Sub(2)
		atom.Inc()
		atom.Dec()
		atom.CAS(1, 0)
		atom.Swap(5)
		atom.Store(1)
	})
}

func TestStressFloat64(t *testing.T) {
	var atom Float64
	runStress(func() {
		atom.Load()
		atom.CAS(1.0, 0.1)
		atom.Add(1.1)
		atom.Sub(0.2)
		atom.Store(1.0)
	})
}

func TestStressBool(t *testing.T) {
	var atom Bool
	runStress(func() {
		atom.Load()
		atom.Store(false)
		atom.Swap(true)
		atom.Load()
		atom.Toggle()
		atom.Toggle()
	})
}

func TestStressString(t *testing.T) {
	var atom String
	runStress(func() {
		atom.Load()
		atom.Store("abc")
		atom.Load()
		atom.Store("def")

	})
}
