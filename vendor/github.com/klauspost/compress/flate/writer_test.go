// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"runtime"
	"testing"
)

func benchmarkEncoder(b *testing.B, testfile, level, n int) {
	b.StopTimer()
	b.SetBytes(int64(n))
	buf0, err := ioutil.ReadFile(testfiles[testfile])
	if err != nil {
		b.Fatal(err)
	}
	if len(buf0) == 0 {
		b.Fatalf("test file %q has no data", testfiles[testfile])
	}
	buf1 := make([]byte, n)
	for i := 0; i < n; i += len(buf0) {
		if len(buf0) > n-i {
			buf0 = buf0[:n-i]
		}
		copy(buf1[i:], buf0)
	}
	buf0 = nil
	runtime.GC()
	w, err := NewWriter(ioutil.Discard, level)
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		w.Reset(ioutil.Discard)
		_, err = w.Write(buf1)
		if err != nil {
			b.Fatal(err)
		}
		err = w.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeDigitsConstant1e4(b *testing.B) { benchmarkEncoder(b, digits, constant, 1e4) }
func BenchmarkEncodeDigitsConstant1e5(b *testing.B) { benchmarkEncoder(b, digits, constant, 1e5) }
func BenchmarkEncodeDigitsConstant1e6(b *testing.B) { benchmarkEncoder(b, digits, constant, 1e6) }
func BenchmarkEncodeDigitsSpeed1e4(b *testing.B)    { benchmarkEncoder(b, digits, speed, 1e4) }
func BenchmarkEncodeDigitsSpeed1e5(b *testing.B)    { benchmarkEncoder(b, digits, speed, 1e5) }
func BenchmarkEncodeDigitsSpeed1e6(b *testing.B)    { benchmarkEncoder(b, digits, speed, 1e6) }
func BenchmarkEncodeDigitsDefault1e4(b *testing.B)  { benchmarkEncoder(b, digits, default_, 1e4) }
func BenchmarkEncodeDigitsDefault1e5(b *testing.B)  { benchmarkEncoder(b, digits, default_, 1e5) }
func BenchmarkEncodeDigitsDefault1e6(b *testing.B)  { benchmarkEncoder(b, digits, default_, 1e6) }
func BenchmarkEncodeDigitsCompress1e4(b *testing.B) { benchmarkEncoder(b, digits, compress, 1e4) }
func BenchmarkEncodeDigitsCompress1e5(b *testing.B) { benchmarkEncoder(b, digits, compress, 1e5) }
func BenchmarkEncodeDigitsCompress1e6(b *testing.B) { benchmarkEncoder(b, digits, compress, 1e6) }
func BenchmarkEncodeTwainConstant1e4(b *testing.B)  { benchmarkEncoder(b, twain, constant, 1e4) }
func BenchmarkEncodeTwainConstant1e5(b *testing.B)  { benchmarkEncoder(b, twain, constant, 1e5) }
func BenchmarkEncodeTwainConstant1e6(b *testing.B)  { benchmarkEncoder(b, twain, constant, 1e6) }
func BenchmarkEncodeTwainSpeed1e4(b *testing.B)     { benchmarkEncoder(b, twain, speed, 1e4) }
func BenchmarkEncodeTwainSpeed1e5(b *testing.B)     { benchmarkEncoder(b, twain, speed, 1e5) }
func BenchmarkEncodeTwainSpeed1e6(b *testing.B)     { benchmarkEncoder(b, twain, speed, 1e6) }
func BenchmarkEncodeTwainDefault1e4(b *testing.B)   { benchmarkEncoder(b, twain, default_, 1e4) }
func BenchmarkEncodeTwainDefault1e5(b *testing.B)   { benchmarkEncoder(b, twain, default_, 1e5) }
func BenchmarkEncodeTwainDefault1e6(b *testing.B)   { benchmarkEncoder(b, twain, default_, 1e6) }
func BenchmarkEncodeTwainCompress1e4(b *testing.B)  { benchmarkEncoder(b, twain, compress, 1e4) }
func BenchmarkEncodeTwainCompress1e5(b *testing.B)  { benchmarkEncoder(b, twain, compress, 1e5) }
func BenchmarkEncodeTwainCompress1e6(b *testing.B)  { benchmarkEncoder(b, twain, compress, 1e6) }

// A writer that fails after N writes.
type errorWriter struct {
	N int
}

func (e *errorWriter) Write(b []byte) (int, error) {
	if e.N <= 0 {
		return 0, io.ErrClosedPipe
	}
	e.N--
	return len(b), nil
}

// Test if errors from the underlying writer is passed upwards.
func TestWriteError(t *testing.T) {
	buf := new(bytes.Buffer)
	n := 65536
	if !testing.Short() {
		n *= 4
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(buf, "asdasfasf%d%dfghfgujyut%dyutyu\n", i, i, i)
	}
	in := buf.Bytes()
	// We create our own buffer to control number of writes.
	copyBuf := make([]byte, 128)
	for l := 0; l < 10; l++ {
		for fail := 1; fail <= 256; fail *= 2 {
			// Fail after 'fail' writes
			ew := &errorWriter{N: fail}
			w, err := NewWriter(ew, l)
			if err != nil {
				t.Fatalf("NewWriter: level %d: %v", l, err)
			}
			n, err := copyBuffer(w, bytes.NewBuffer(in), copyBuf)
			if err == nil {
				t.Fatalf("Level %d: Expected an error, writer was %#v", l, ew)
			}
			n2, err := w.Write([]byte{1, 2, 2, 3, 4, 5})
			if n2 != 0 {
				t.Fatal("Level", l, "Expected 0 length write, got", n)
			}
			if err == nil {
				t.Fatal("Level", l, "Expected an error")
			}
			err = w.Flush()
			if err == nil {
				t.Fatal("Level", l, "Expected an error on flush")
			}
			err = w.Close()
			if err == nil {
				t.Fatal("Level", l, "Expected an error on close")
			}

			w.Reset(ioutil.Discard)
			n2, err = w.Write([]byte{1, 2, 3, 4, 5, 6})
			if err != nil {
				t.Fatal("Level", l, "Got unexpected error after reset:", err)
			}
			if n2 == 0 {
				t.Fatal("Level", l, "Got 0 length write, expected > 0")
			}
			if testing.Short() {
				return
			}
		}
	}
}

func TestDeterministicL1(t *testing.T)  { testDeterministic(1, t) }
func TestDeterministicL2(t *testing.T)  { testDeterministic(2, t) }
func TestDeterministicL3(t *testing.T)  { testDeterministic(3, t) }
func TestDeterministicL4(t *testing.T)  { testDeterministic(4, t) }
func TestDeterministicL5(t *testing.T)  { testDeterministic(5, t) }
func TestDeterministicL6(t *testing.T)  { testDeterministic(6, t) }
func TestDeterministicL7(t *testing.T)  { testDeterministic(7, t) }
func TestDeterministicL8(t *testing.T)  { testDeterministic(8, t) }
func TestDeterministicL9(t *testing.T)  { testDeterministic(9, t) }
func TestDeterministicL0(t *testing.T)  { testDeterministic(0, t) }
func TestDeterministicLM2(t *testing.T) { testDeterministic(-2, t) }

func testDeterministic(i int, t *testing.T) {
	// Test so much we cross a good number of block boundaries.
	var length = maxStoreBlockSize*30 + 500
	if testing.Short() {
		length /= 10
	}

	// Create a random, but compressible stream.
	rng := rand.New(rand.NewSource(1))
	t1 := make([]byte, length)
	for i := range t1 {
		t1[i] = byte(rng.Int63() & 7)
	}

	// Do our first encode.
	var b1 bytes.Buffer
	br := bytes.NewBuffer(t1)
	w, err := NewWriter(&b1, i)
	if err != nil {
		t.Fatal(err)
	}
	// Use a very small prime sized buffer.
	cbuf := make([]byte, 787)
	_, err = copyBuffer(w, br, cbuf)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	// We choose a different buffer size,
	// bigger than a maximum block, and also a prime.
	var b2 bytes.Buffer
	cbuf = make([]byte, 81761)
	br2 := bytes.NewBuffer(t1)
	w2, err := NewWriter(&b2, i)
	if err != nil {
		t.Fatal(err)
	}
	_, err = copyBuffer(w2, br2, cbuf)
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()

	b1b := b1.Bytes()
	b2b := b2.Bytes()

	if !bytes.Equal(b1b, b2b) {
		t.Errorf("level %d did not produce deterministic result, result mismatch, len(a) = %d, len(b) = %d", i, len(b1b), len(b2b))
	}

	// Test using io.WriterTo interface.
	var b3 bytes.Buffer
	br = bytes.NewBuffer(t1)
	w, err = NewWriter(&b3, i)
	if err != nil {
		t.Fatal(err)
	}
	_, err = br.WriteTo(w)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	b3b := b3.Bytes()
	if !bytes.Equal(b1b, b3b) {
		t.Errorf("level %d (io.WriterTo) did not produce deterministic result, result mismatch, len(a) = %d, len(b) = %d", i, len(b1b), len(b3b))
	}
}

// copyBuffer is a copy of io.CopyBuffer, since we want to support older go versions.
// This is modified to never use io.WriterTo or io.ReaderFrom interfaces.
func copyBuffer(dst io.Writer, src io.Reader, buf []byte) (written int64, err error) {
	if buf == nil {
		buf = make([]byte, 32*1024)
	}
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			err = er
			break
		}
	}
	return written, err
}
