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
	"testing"
)

const _parallelism = 4
const _iterations = 1000

func TestStressInt32(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(_parallelism))
	atom := &Int32{0}
	for i := 0; i < _parallelism; i++ {
		go func() {
			for j := 0; j < _iterations; j++ {
				atom.Load()
				atom.Add(1)
				atom.Sub(2)
				atom.Inc()
				atom.Dec()
				atom.CAS(1, 0)
				atom.Swap(5)
				atom.Store(1)
			}
		}()
	}
}

func TestStressInt64(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(_parallelism))
	atom := &Int64{0}
	for i := 0; i < _parallelism; i++ {
		go func() {
			for j := 0; j < _iterations; j++ {
				atom.Load()
				atom.Add(1)
				atom.Sub(2)
				atom.Inc()
				atom.Dec()
				atom.CAS(1, 0)
				atom.Swap(5)
				atom.Store(1)
			}
		}()
	}
}

func TestStressUint32(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(_parallelism))
	atom := &Uint32{0}
	for i := 0; i < _parallelism; i++ {
		go func() {
			for j := 0; j < _iterations; j++ {
				atom.Load()
				atom.Add(1)
				atom.Sub(2)
				atom.Inc()
				atom.Dec()
				atom.CAS(1, 0)
				atom.Swap(5)
				atom.Store(1)
			}
		}()
	}
}

func TestStressUint64(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(_parallelism))
	atom := &Uint64{0}
	for i := 0; i < _parallelism; i++ {
		go func() {
			for j := 0; j < _iterations; j++ {
				atom.Load()
				atom.Add(1)
				atom.Sub(2)
				atom.Inc()
				atom.Dec()
				atom.CAS(1, 0)
				atom.Swap(5)
				atom.Store(1)
			}
		}()
	}
}

func TestStressBool(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(_parallelism))
	atom := NewBool(false)
	for i := 0; i < _parallelism; i++ {
		go func() {
			for j := 0; j < _iterations; j++ {
				atom.Load()
				atom.Store(false)
				atom.Swap(true)
				atom.Load()
				atom.Toggle()
				atom.Toggle()
			}
		}()
	}
}

func TestStressString(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(_parallelism))
	atom := NewString("")
	for i := 0; i < _parallelism; i++ {
		go func() {
			for j := 0; j < _iterations; j++ {
				atom.Load()
				atom.Store("abc")
				atom.Load()
				atom.Store("def")
			}
		}()
	}
}
