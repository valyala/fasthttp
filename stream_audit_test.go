package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
)

func TestStreamAuditWriterBoundariesAndResume(t *testing.T) {
	sentinel := errors.New("audit destination failed")
	for _, tc := range []struct {
		name   string
		failAt int
		n      int
		full   bool
		err    error
	}{
		{"zeroNil", 1, 0, false, nil},
		{"partialNil", 1, 3, false, nil},
		{"zeroError", 1, 0, false, sentinel},
		{"fullError", 1, 0, true, sentinel},
		{"multiBlockPartialError", 2, 17, false, sentinel},
		{"multiBlockFullError", 2, 0, true, sentinel},
	} {
		for _, resume := range []string{"Read", "WriteTo"} {
			t.Run(tc.name+"/"+resume, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					source := make([]byte, 100_000)
					for i := range source {
						source[i] = byte(i * 31)
					}
					sw, producerDone := streamCompatProducer(source, 64*1024, 1)
					r := NewBoundedStreamReader(sw)
					defer streamClose(t, r)
					wt, ok := r.(io.WriterTo)
					if !ok {
						t.Fatal("bounded reader lacks WriterTo")
					}
					w := streamAuditBoundaryWriter{failAt: tc.failAt, n: tc.n, full: tc.full, err: tc.err}
					n, err := wt.WriteTo(&w)
					wantErr := tc.err
					if wantErr == nil {
						wantErr = io.ErrShortWrite
					}
					if !errors.Is(err, wantErr) || n != int64(w.dst.Len()) || w.calls != tc.failAt {
						t.Fatalf("WriteTo = %d, %v; accepted = %d; calls = %d", n, err, w.dst.Len(), w.calls)
					}
					if !bytes.Equal(w.dst.Bytes(), source[:n]) {
						t.Fatal("accepted prefix differs from source")
					}
					var tail []byte
					if resume == "Read" {
						tail, err = io.ReadAll(r)
					} else {
						var dst bytes.Buffer
						var copied int64
						copied, err = wt.WriteTo(&dst)
						tail = dst.Bytes()
						if copied != int64(len(tail)) {
							t.Fatalf("resumed WriteTo count = %d, bytes = %d", copied, len(tail))
						}
					}
					if err != nil || !bytes.Equal(tail, source[n:]) {
						t.Fatalf("resumed %s: %d bytes, %v; want %d bytes", resume, len(tail), err, len(source)-int(n))
					}
					if err := <-producerDone; err != nil {
						t.Fatal(err)
					}
				})
			})
		}
	}
}

type streamAuditBoundaryWriter struct {
	dst    bytes.Buffer
	failAt int
	n      int
	full   bool
	err    error
	calls  int
}

func (w *streamAuditBoundaryWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls != w.failAt {
		return w.dst.Write(p)
	}
	n := min(w.n, len(p))
	if w.full {
		n = len(p)
	}
	w.dst.Write(p[:n])
	return n, w.err
}

func TestStreamAuditEmptyReadCloseWakeup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finishProducer := make(chan struct{})
		r := NewBoundedStreamReader(func(w *bufio.Writer) { <-finishProducer })
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() {
			n, err := r.Read(make([]byte, 1))
			done <- result{n, err}
		}()
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("empty live reader did not block: %d, %v", got.n, got.err)
		default:
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.n != 0 || !errors.Is(got.err, io.EOF) {
			t.Fatalf("empty closed reader = %d, %v", got.n, got.err)
		}
		close(finishProducer)
	})
}

func TestStreamAuditReadyReadDoesNotWaitForFutureData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready, finish := make(chan error, 1), make(chan struct{})
		r := streamAuditGatedBurst(ready, finish)
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		first := make([]byte, 512)
		if _, err := io.ReadFull(r, first); err != nil || !bytes.Equal(first, bytes.Repeat([]byte("v"), 512)) {
			t.Fatalf("ready data = %q, %v", first, err)
		}
		close(finish)
		tail, err := io.ReadAll(r)
		if err != nil || len(tail) != 0 {
			t.Fatalf("tail = %d, %v", len(tail), err)
		}
		streamClose(t, r)
	})
}

func TestStreamAuditReadyHTTPBurstFlushBeforeFutureData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready, finish := make(chan error, 1), make(chan struct{})
		r := streamAuditGatedBurst(ready, finish)
		defer streamClose(t, r)
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		server, client := net.Pipe()
		defer streamClose(t, client)
		done := make(chan error, 1)
		go func() {
			defer streamClose(t, server)
			var resp Response
			resp.Header.noDefaultDate = true
			resp.SetBodyStream(r, -1)
			bw := bufio.NewWriterSize(server, 64*1024)
			err := resp.Write(bw)
			if err == nil {
				err = bw.Flush()
			}
			done <- err
		}()
		response, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer streamClose(t, response.Body)
		got := make([]byte, 512)
		if _, err := io.ReadFull(response.Body, got); err != nil || !bytes.Equal(got, bytes.Repeat([]byte("v"), 512)) {
			t.Fatalf("ready HTTP data=%d,%v", len(got), err)
		}
		close(finish)
		if tail, err := io.ReadAll(response.Body); err != nil || len(tail) != 0 {
			t.Fatal(len(tail), err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func streamAuditGatedBurst(ready chan<- error, finish <-chan struct{}) io.ReadCloser {
	return NewBoundedStreamReader(func(w *bufio.Writer) {
		for range 4 {
			if _, err := w.Write(bytes.Repeat([]byte("v"), 128)); err != nil {
				ready <- err
				return
			}
			if err := w.Flush(); err != nil {
				ready <- err
				return
			}
		}
		ready <- nil
		<-finish
	})
}
