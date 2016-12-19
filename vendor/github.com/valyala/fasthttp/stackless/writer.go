package stackless

import (
	"fmt"
	"github.com/valyala/bytebufferpool"
	"io"
	"runtime"
)

// Writer is an interface stackless writer must conform to.
//
// The interface contains common subset for Writers from compress/* packages.
type Writer interface {
	Write(p []byte) (int, error)
	Flush() error
	Close() error
	Reset(w io.Writer)
}

// NewWriterFunc must return new writer that will be wrapped into
// stackless writer.
type NewWriterFunc func(w io.Writer) Writer

// NewWriter creates a stackless writer around a writer returned
// from newWriter.
//
// The returned writer writes data to dstW.
//
// Writers that use a lot of stack space may be wrapped into stackless writer,
// thus saving stack space for high number of concurrently running goroutines.
func NewWriter(dstW io.Writer, newWriter NewWriterFunc) Writer {
	w := &writer{
		dstW: dstW,
		done: make(chan error),
	}
	w.zw = newWriter(&w.xw)
	return w
}

type writer struct {
	dstW io.Writer
	zw   Writer
	xw   xWriter

	done chan error
	n    int

	p  []byte
	op op
}

type op int

const (
	opWrite op = iota
	opFlush
	opClose
	opReset
)

func (w *writer) Write(p []byte) (int, error) {
	w.p = p
	err := w.do(opWrite)
	w.p = nil
	return w.n, err
}

func (w *writer) Flush() error {
	return w.do(opFlush)
}

func (w *writer) Close() error {
	return w.do(opClose)
}

func (w *writer) Reset(dstW io.Writer) {
	w.xw.Reset()
	w.do(opReset)
	w.dstW = dstW
}

func (w *writer) do(op op) error {
	w.op = op
	writerCh <- w
	err := <-w.done
	if err != nil {
		return err
	}
	if w.xw.bb != nil && len(w.xw.bb.B) > 0 {
		_, err = w.dstW.Write(w.xw.bb.B)
	}
	w.xw.Reset()

	return err
}

type xWriter struct {
	bb *bytebufferpool.ByteBuffer
}

func (w *xWriter) Write(p []byte) (int, error) {
	if w.bb == nil {
		w.bb = bufferPool.Get()
	}
	w.bb.Write(p)
	return len(p), nil
}

func (w *xWriter) Reset() {
	if w.bb != nil {
		bufferPool.Put(w.bb)
		w.bb = nil
	}
}

var bufferPool bytebufferpool.Pool

func init() {
	n := runtime.GOMAXPROCS(-1)
	writerCh = make(chan *writer, n)
	for i := 0; i < n; i++ {
		go worker()
	}
}

var writerCh chan *writer

func worker() {
	var err error
	for w := range writerCh {
		switch w.op {
		case opWrite:
			w.n, err = w.zw.Write(w.p)
		case opFlush:
			err = w.zw.Flush()
		case opClose:
			err = w.zw.Close()
		case opReset:
			w.zw.Reset(&w.xw)
			err = nil
		default:
			panic(fmt.Sprintf("BUG: unexpected op: %d", w.op))
		}
		w.done <- err
	}
}
