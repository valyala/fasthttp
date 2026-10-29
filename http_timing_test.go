package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
)

func BenchmarkCopyZeroAllocOSFileToBytesBuffer(b *testing.B) {
	r, err := os.Open("./README.md")
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()

	buf := &bytes.Buffer{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		_, err = copyZeroAlloc(buf, r)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCopyZeroAllocBytesBufferToOSFile(b *testing.B) {
	f, err := os.Open("./README.md")
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	buf := &bytes.Buffer{}
	_, err = io.Copy(buf, f)
	if err != nil {
		b.Fatal(err)
	}

	tmp, err := os.CreateTemp(os.TempDir(), "test_*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.Remove(tmp.Name())

	w, err := os.OpenFile(tmp.Name(), os.O_WRONLY, 0o444)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := w.Seek(0, 0)
		if err != nil {
			b.Fatal(err)
		}
		_, err = copyZeroAlloc(w, buf)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCopyZeroAllocOSFileToStringsBuilder(b *testing.B) {
	r, err := os.Open("./README.md")
	if err != nil {
		b.Fatalf("Failed to open testing file: %v", err)
	}
	defer r.Close()

	w := &strings.Builder{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		_, err = copyZeroAlloc(w, r)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCopyZeroAllocIOLimitedReaderToOSFile(b *testing.B) {
	f, err := os.Open("./README.md")
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()

	r := io.LimitReader(f, 1024)

	tmp, err := os.CreateTemp(os.TempDir(), "test_*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.Remove(tmp.Name())

	w, err := os.OpenFile(tmp.Name(), os.O_WRONLY, 0o444)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := w.Seek(0, 0)
		if err != nil {
			b.Fatal(err)
		}
		_, err = copyZeroAlloc(w, r)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCopyZeroAllocOSFileToOSFile(b *testing.B) {
	r, err := os.Open("./README.md")
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()

	f, err := os.CreateTemp(os.TempDir(), "test_*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.Remove(f.Name())

	w, err := os.OpenFile(f.Name(), os.O_WRONLY, 0o444)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := w.Seek(0, 0)
		if err != nil {
			b.Fatal(err)
		}
		_, err = copyZeroAlloc(w, r)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCopyZeroAllocOSFileToNetConn(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}

	addr := ln.Addr().String()
	defer ln.Close()

	done := make(chan struct{})
	defer close(done)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			b.Error(err)
			return
		}
		defer conn.Close()
		for {
			select {
			case <-done:
				return
			default:
				_, err := io.Copy(io.Discard, conn)
				if err != nil {
					b.Error(err)
					return
				}
			}
		}
	}()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	file, err := os.Open("./README.md")
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := copyZeroAlloc(conn, file); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCopyZeroAllocNetConnToOSFile(b *testing.B) {
	data, err := os.ReadFile("./README.md")
	if err != nil {
		b.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}

	addr := ln.Addr().String()
	defer ln.Close()

	done := make(chan struct{})
	defer close(done)

	writeDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				conn, err := ln.Accept()
				if err != nil {
					b.Error(err)
					return
				}
				_, err = conn.Write(data)
				if err != nil {
					b.Error(err)
				}
				conn.Close()
				writeDone <- struct{}{}
			}
		}
	}()

	tmp, err := os.CreateTemp(os.TempDir(), "test_*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.Remove(tmp.Name())

	file, err := os.OpenFile(tmp.Name(), os.O_WRONLY, 0o444)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		<-writeDone
		_, err = file.Seek(0, 0)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		_, err = copyZeroAlloc(file, conn)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		conn, err = net.Dial("tcp", addr)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadBodyUnlimited(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20, 4 << 20, 16 << 20, 64 << 20, 256 << 20} {
		for _, chunked := range []bool{false, true} {
			for _, reuse := range []bool{false, true} {
				name := fmt.Sprintf("size=%dKiB/chunked=%t/reuse=%t", size>>10, chunked, reuse)
				b.Run(name, func(b *testing.B) {
					benchmarkReadBodyUnlimited(b, size, chunked, reuse)
				})
			}
		}
	}
}

func benchmarkReadBodyUnlimited(b *testing.B, size int, chunked, reuse bool) {
	in := createFixedBody(size)
	if chunked {
		in = fmt.Appendf(nil, "%x\r\n%s\r\n0\r\n\r\n", size, in)
	}
	var dst []byte
	if reuse {
		dst = make([]byte, 0, size+len(strCRLF))
	}
	var r bytes.Reader
	br := bufio.NewReader(&r)

	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		r.Reset(in)
		br.Reset(&r)
		var err error
		if chunked {
			_, err = readBodyChunked(br, 0, dst)
		} else {
			_, err = readBody(br, size, 0, dst)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}
