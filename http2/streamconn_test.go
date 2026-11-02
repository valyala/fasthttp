package http2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

func openRawTunnel(peer *rawPeer, streamID uint32) {
	peer.writeHeaders(streamID, false,
		[2]string{":method", fasthttp.MethodConnect},
		[2]string{":protocol", "websocket"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/ws"},
	)
}

func isDataOn(streamID uint32) func(xhttp2.Frame) bool {
	return func(frame xhttp2.Frame) bool {
		data, ok := frame.(*xhttp2.DataFrame)
		return ok && data.StreamID == streamID
	}
}

func assertCloseUnblocks(t *testing.T, conn fasthttp.StreamConn, written <-chan error) {
	t.Helper()
	closed := make(chan error, 1)
	go func() { closed <- conn.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() waited for a Write blocked on flow control")
	}
	select {
	case err := <-written:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("blocked Write() after Close() = %v, want %v", err, net.ErrClosed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() didn't unblock the Write()")
	}
}

// The server closes a StreamConn when its handler returns, which must not wait
// for a Write the handler left blocked.
func TestStreamHandlerReturnEndsStreamWithBlockedWrite(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			go func() { _, _ = conn.Write([]byte("payload")) }()
			time.Sleep(50 * time.Millisecond)
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	openRawTunnel(peer, 1)
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.RSTStreamFrame:
			return frame.StreamID == 1
		case *xhttp2.DataFrame:
			return frame.StreamID == 1 && frame.StreamEnded()
		}
		return false
	})
}

// readDeadlineLeavesStreamUsable times out a Read, runs peerWrites, which must
// make the peer send "later", and reads it with the deadline cleared.
func readDeadlineLeavesStreamUsable(conn fasthttp.StreamConn, peerWrites func() error) error {
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		return err
	}
	_, err := conn.Read(make([]byte, 1))
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("Read() past the deadline = %v, want a timeout wrapping %v", err, os.ErrDeadlineExceeded)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	if err := peerWrites(); err != nil {
		return fmt.Errorf("writing after the read deadline: %w", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "later" {
		return fmt.Errorf("ReadFull() after clearing the deadline = %q, %v", buf, err)
	}
	return nil
}

// A deadline notification delayed until its write ended and another began must
// leave the new write alone.
func TestStaleWriteInterruptLeavesNewWrite(t *testing.T) {
	sc := &serverConn{ctx: context.Background(), commands: make(chan serverCommand, 4), streams: make(map[uint32]*serverStream)}
	stream := newServerStream(sc, 1)
	sc.streams[1] = stream
	conn := newStreamConn(sc, stream)
	old, err := conn.beginWrite()
	if err != nil {
		t.Fatal(err)
	}
	first := &streamWrite{result: make(chan streamWriteResult, 1), cut: old.done}
	conn.inflight = first
	stream.pendingWrite = first
	stream.pendingData = []byte("old")
	stream.responseBytes = 3
	entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	conn.wakeWrite = func() {
		close(entered)
		<-resume
		conn.interruptWrite()
	}
	conn.mu.Lock()
	conn.writeDeadline.at = time.Now().Add(-time.Second)
	conn.mu.Unlock()
	go func() {
		conn.expire(&conn.writeDeadline)
		close(finished)
	}()
	<-entered
	// The owner sees the cut on its own at the next frame.
	sc.cutPendingWrite(stream)
	<-first.result
	conn.endWrite()
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	fresh, err := conn.beginWrite()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.endWrite()
	second := &streamWrite{result: make(chan streamWriteResult, 1), cut: fresh.done}
	conn.mu.Lock()
	conn.inflight = second
	conn.mu.Unlock()
	stream.pendingWrite = second
	stream.pendingData = []byte("new payload")
	stream.responseBytes = int64(len(stream.pendingData))
	close(resume)
	<-finished
	command := <-sc.commands
	if err := sc.processCommand(&command); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-second.result:
		t.Fatalf("the stale notification ended the new write: %d, %v", result.n, result.err)
	default:
	}
}
