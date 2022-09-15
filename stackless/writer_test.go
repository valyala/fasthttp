package stackless

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestCompressFlateSerial(t *testing.T) {
	t.Parallel()

	if err := testCompressFlate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompressFlateConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(testCompressFlate, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func testCompressFlate() error {
	return testWriter(func(w io.Writer) Writer {
		zw, err := flate.NewWriter(w, flate.DefaultCompression)
		if err != nil {
			panic(fmt.Sprintf("BUG: unexpected error: %v", err))
		}
		return zw
	}, func(r io.Reader) io.Reader {
		return flate.NewReader(r)
	})
}

func TestCompressGzipSerial(t *testing.T) {
	t.Parallel()

	if err := testCompressGzip(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompressGzipConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(testCompressGzip, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func testCompressGzip() error {
	return testWriter(func(w io.Writer) Writer {
		return gzip.NewWriter(w)
	}, func(r io.Reader) io.Reader {
		zr, err := gzip.NewReader(r)
		if err != nil {
			panic(fmt.Sprintf("BUG: cannot create gzip reader: %v", err))
		}
		return zr
	})
}

func testWriter(newWriter NewWriterFunc, newReader func(io.Reader) io.Reader) error {
	dstW := &bytes.Buffer{}
	w := NewWriter(dstW, newWriter)

	for i := 0; i < 5; i++ {
		if err := testWriterReuse(w, dstW, newReader); err != nil {
			return fmt.Errorf("unexpected error when re-using writer on iteration %d: %w", i, err)
		}
		dstW = &bytes.Buffer{}
		w.Reset(dstW)
	}

	return nil
}

func testWriterReuse(w Writer, r io.Reader, newReader func(io.Reader) io.Reader) error {
	wantW := &bytes.Buffer{}
	mw := io.MultiWriter(w, wantW)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(mw, "foobar %d\n", i)
		if i%13 == 0 {
			if err := w.Flush(); err != nil {
				return fmt.Errorf("error on flush: %w", err)
			}
		}
	}
	w.Close()

	zr := newReader(r)
	data, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("unexpected error: %w, data=%q", err, data)
	}

	wantData := wantW.Bytes()
	if !bytes.Equal(data, wantData) {
		return fmt.Errorf("unexpected data: %q. Expecting %q", data, wantData)
	}

	return nil
}

func testConcurrent(testFunc func() error, concurrency int) error {
	ch := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			ch <- testFunc()
		}()
	}
	for i := 0; i < concurrency; i++ {
		select {
		case err := <-ch:
			if err != nil {
				return fmt.Errorf("unexpected error on goroutine %d: %w", i, err)
			}
		case <-time.After(time.Second):
			return fmt.Errorf("timeout on goroutine %d", i)
		}
	}
	return nil
}
