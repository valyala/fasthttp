package http2

import (
	"bytes"
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
	"golang.org/x/net/http2/hpack"
)

func openTestStreamConn(t *testing.T, testServer *testServer, hc *fasthttp.HostClient) fasthttp.StreamConn {
	t.Helper()
	if hc == nil {
		hc = &fasthttp.HostClient{}
	}
	hc.Addr = testServer.listener.Addr().String()
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, EnableExtendedConnect: true}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	var req fasthttp.Request
	var resp fasthttp.Response
	req.Header.SetMethod(fasthttp.MethodConnect)
	req.Header.SetConnectProtocol("websocket")
	req.SetRequestURI(testServer.URL("/ws"))
	conn, err := hc.OpenStream(&req, &resp)
	if err != nil {
		t.Fatalf("OpenStream() error: %v", err)
	}
	return conn
}

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

// net.Conn: Close unblocks a blocked Write, here one waiting for flow control.
func TestStreamConnCloseUnblocksBlockedWrite(t *testing.T) {
	t.Run("client", func(t *testing.T) {
		arrived := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				_, _ = io.ReadFull(conn, make([]byte, 1))
				close(arrived)
				<-release
			})
		}}
		testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true, MaxUploadBufferPerStream: 16})
		conn := openTestStreamConn(t, testServer, nil)
		written := make(chan error, 1)
		go func() {
			_, err := conn.Write(make([]byte, 64))
			written <- err
		}()
		<-arrived
		assertCloseUnblocks(t, conn, written)
	})
	t.Run("server", func(t *testing.T) {
		conns := make(chan fasthttp.StreamConn, 1)
		finished := make(chan struct{})
		defer close(finished)
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				conns <- conn
				<-finished
			})
		}}
		testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1}); err != nil {
			t.Fatalf("WriteSettings() error: %v", err)
		}
		openRawTunnel(peer, 1)
		conn := <-conns
		written := make(chan error, 1)
		go func() {
			_, err := conn.Write([]byte("payload"))
			written <- err
		}()
		peer.readHeaders(1, isDataOn(1))
		assertCloseUnblocks(t, conn, written)
	})
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

// A read deadline interrupts Reads only: the stream stays open, and data that
// arrives later is read once the deadline is cleared.
func TestStreamConnReadDeadlineLeavesStreamUsable(t *testing.T) {
	t.Run("client", func(t *testing.T) {
		received := make(chan []byte, 1)
		send := make(chan struct{})
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				buf := make([]byte, 5)
				_, _ = io.ReadFull(conn, buf)
				received <- buf
				<-send
				_, _ = conn.Write([]byte("later"))
				_, _ = io.Copy(io.Discard, conn)
			})
		}}
		testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
		conn := openTestStreamConn(t, testServer, nil)
		defer conn.Close()
		err := readDeadlineLeavesStreamUsable(conn, func() error {
			if _, err := conn.Write([]byte("hello")); err != nil {
				return err
			}
			if got := <-received; string(got) != "hello" {
				return errors.New("peer read " + string(got))
			}
			close(send)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("server", func(t *testing.T) {
		result := make(chan error, 1)
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				result <- readDeadlineLeavesStreamUsable(conn, func() error {
					_, err := conn.Write([]byte("hello"))
					return err
				})
			})
		}}
		testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
		conn := openTestStreamConn(t, testServer, nil)
		defer conn.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
			t.Fatalf("client ReadFull() = %q, %v", buf, err)
		}
		if _, err := conn.Write([]byte("later")); err != nil {
			t.Fatalf("client Write() error: %v", err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
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

// A write deadline ends a Write at the bytes already sent and leaves the stream
// open, so the rest can follow once the deadline is refreshed.
func TestStreamConnPartialWriteResumes(t *testing.T) {
	payload := []byte("0123456789")
	type writeResult struct {
		n   int
		err error
	}
	assertTimedOut := func(t *testing.T, got writeResult) {
		t.Helper()
		if got.n <= 0 || got.n >= len(payload) || !isTimeout(got.err) || !errors.Is(got.err, os.ErrDeadlineExceeded) {
			t.Fatalf("Write() past the deadline = %d, %v; want a partial count and a deadline error", got.n, got.err)
		}
	}
	t.Run("server", func(t *testing.T) {
		first := make(chan writeResult, 1)
		second := make(chan writeResult, 1)
		resume := make(chan struct{})
		finished := make(chan struct{})
		defer close(finished)
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
				n, err := conn.Write(payload)
				first <- writeResult{n, err}
				<-resume
				_ = conn.SetWriteDeadline(time.Time{})
				m, err := conn.Write(payload[n:])
				second <- writeResult{m, err}
				<-finished
			})
		}}
		testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 4}); err != nil {
			t.Fatalf("WriteSettings() error: %v", err)
		}
		openRawTunnel(peer, 1)
		var got []byte
		readData := func(want int) {
			_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for len(got) < want {
				frame, err := peer.framer.ReadFrame()
				if err != nil {
					t.Fatalf("ReadFrame() error: %v", err)
				}
				switch frame := frame.(type) {
				case *xhttp2.DataFrame:
					if frame.StreamID == 1 {
						got = append(got, frame.Data()...)
					}
				case *xhttp2.RSTStreamFrame:
					t.Fatalf("stream reset by %v", frame.ErrCode)
				}
			}
		}
		readData(4)
		result := <-first
		assertTimedOut(t, result)
		if result.n != len(got) {
			t.Fatalf("Write() reported %d bytes, peer got %d", result.n, len(got))
		}
		if err := peer.framer.WriteWindowUpdate(1, 1<<16); err != nil {
			t.Fatalf("WriteWindowUpdate() error: %v", err)
		}
		close(resume)
		readData(len(payload))
		if result := <-second; result.n != len(payload)-4 || result.err != nil {
			t.Fatalf("resumed Write() = %d, %v", result.n, result.err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("peer got %q, want %q", got, payload)
		}
	})
	t.Run("client", func(t *testing.T) {
		received := make(chan []byte, 1)
		release := make(chan struct{})
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				<-release
				buf := make([]byte, len(payload)+64)
				n, _ := io.ReadAtLeast(conn, buf, len(payload)+64)
				received <- buf[:n]
			})
		}}
		// A small upload window stalls the write mid-payload.
		testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true, MaxUploadBufferPerStream: 16})
		conn := openTestStreamConn(t, testServer, nil)
		defer conn.Close()
		long := append(bytes.Repeat([]byte{'-'}, 64), payload...)
		_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := conn.Write(long)
		if n <= 0 || n >= len(long) || !isTimeout(err) || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Write() past the deadline = %d, %v; want a partial count and a deadline error", n, err)
		}
		_ = conn.SetWriteDeadline(time.Time{})
		close(release)
		if m, err := conn.Write(long[n:]); m != len(long)-n || err != nil {
			t.Fatalf("resumed Write() = %d, %v", m, err)
		}
		if got := <-received; !bytes.Equal(got, long) {
			t.Fatalf("peer got %q, want %q", got, long)
		}
	})
}

// CloseWrite is a write: its deadline bounds the wait for the connection's
// write slot, and a CloseWrite cut short leaves the write side open.
func TestClientStreamConnCloseWriteHonorsWriteDeadline(t *testing.T) {
	newConn := func(t *testing.T) (*clientStreamConn, *clientConn) {
		cc := installTestWriter(t, &clientConn{
			streams: make(map[uint32]*clientStream),
			pool:    &clientPool{},
		})
		stream := &clientStream{
			id: 1, conn: cc, isOpenStream: true, requestStarted: true, responseHeader: true,
			result: make(chan clientResult, 1),
		}
		stream.responseBody = newResponseBody(cc, 1)
		cc.streams[1] = stream
		cc.activeStreams = 2 // another stream keeps the connection busy
		return newClientStreamConn(cc.conn, stream), cc
	}
	t.Run("CloseWrite", func(t *testing.T) {
		conn, cc := newConn(t)
		cc.writeSem <- struct{}{} // another producer holds the write slot
		_ = conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		done := make(chan error, 1)
		go func() { done <- conn.CloseWrite() }()
		select {
		case err := <-done:
			if !isTimeout(err) {
				t.Fatalf("CloseWrite() = %v, want timeout", err)
			}
		case <-time.After(2 * time.Second):
			<-cc.writeSem
			t.Fatal("CloseWrite() waited past its deadline")
		}
		<-cc.writeSem
		_ = conn.SetWriteDeadline(time.Time{})
		if err := conn.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite() once the slot is free: %v", err)
		}
	})
	t.Run("Close", func(t *testing.T) {
		conn, cc := newConn(t)
		cc.writeSem <- struct{}{}
		_ = conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		done := make(chan error, 1)
		go func() { done <- conn.Close() }()
		time.Sleep(100 * time.Millisecond)
		<-cc.writeSem // the reset needs the slot too
		if err := <-done; err != nil {
			t.Fatalf("Close() error: %v", err)
		}
		cc.mu.Lock()
		err := conn.stream.err
		cc.mu.Unlock()
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("stream error after Close() past the deadline = %v, want a reset with %v", err, net.ErrClosed)
		}
	})
}

// After Close, data the peer still sends resets the stream, which stops the
// peer's writes instead of letting them run into a discarded body.
func TestClientStreamConnCloseStopsPeerWrites(t *testing.T) {
	serverErr := make(chan error, 1)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			chunk := make([]byte, 1024)
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if _, err := conn.Write(chunk); err != nil {
					serverErr <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
			serverErr <- nil
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	conn := openTestStreamConn(t, testServer, nil)
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case err := <-serverErr:
		if err == nil {
			t.Fatal("the server kept writing to a closed StreamConn")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server kept writing to a closed StreamConn")
	}
}

func TestClientStreamConnCloseAfterPeerFinishedReturnsNil(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			_, _ = conn.Write([]byte("bye"))
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	conn := openTestStreamConn(t, testServer, nil)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if got, err := io.ReadAll(conn); err != nil || string(got) != "bye" {
		t.Fatalf("ReadAll() = %q, %v", got, err)
	}
	// The server ended its side and then reset the client's.
	time.Sleep(50 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() after the peer finished = %v", err)
	}
}

func TestStreamConnZeroLengthWriteAfterClose(t *testing.T) {
	result := make(chan error, 1)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			_ = conn.Close()
			_, err := conn.Write(nil)
			result <- err
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	conn := openTestStreamConn(t, testServer, nil)
	defer conn.Close()
	if err := <-result; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("server Write(nil) after Close() = %v, want %v", err, net.ErrClosed)
	}
	_ = conn.Close()
	if _, err := conn.Write(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("client Write(nil) after Close() = %v, want %v", err, net.ErrClosed)
	}
}

// Tunnel bytes aren't a response body, so MaxResponseBodySize doesn't cap them.
func TestOpenStreamIgnoresMaxResponseBodySize(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 4096)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			_, _ = conn.Write(payload)
			_ = conn.CloseWrite()
			_, _ = io.Copy(io.Discard, conn)
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	conn := openTestStreamConn(t, testServer, &fasthttp.HostClient{MaxResponseBodySize: 1024})
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if got, err := io.ReadAll(conn); err != nil || len(got) != len(payload) {
		t.Fatalf("ReadAll() = %d bytes, %v; want %d", len(got), err, len(payload))
	}
}

// A result the read loop posted as the deadline fired stands, and the stream it
// carries stays open.
func TestWaitResultKeepsAResultPostedAtTheDeadline(t *testing.T) {
	for range 100 {
		c := installTestWriter(t, &clientConn{
			streams: make(map[uint32]*clientStream),
			pool:    &clientPool{},
		})
		stream := &clientStream{
			id: 1, conn: c, isOpenStream: true, requestStarted: true,
			result: make(chan clientResult, 1),
		}
		c.streams[1] = stream
		c.activeStreams = 2 // another stream keeps the connection busy
		c.mu.Lock()
		c.sendResultLocked(stream, clientResult{})
		c.mu.Unlock()
		result := c.waitResult(stream, time.Now().Add(-time.Second), failureReadTimeout)
		c.mu.Lock()
		streamErr := stream.err
		c.mu.Unlock()
		if result.err != nil || streamErr != nil {
			t.Fatalf("waitResult() = %v with the stream failed by %v, want the posted result", result.err, streamErr)
		}
	}
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

func TestClientStreamConnCloseWithExpiredDeadlineResets(t *testing.T) {
	cc := installTestWriter(t, &clientConn{
		streams: make(map[uint32]*clientStream),
		pool:    &clientPool{},
	})
	cc.activeStreams = 1 // another stream keeps the connection busy
	for id := uint32(1); id <= 400; id += 2 {
		stream := &clientStream{
			id: id, conn: cc, isOpenStream: true, requestStarted: true, responseHeader: true,
			result: make(chan clientResult, 1),
		}
		stream.responseBody = newResponseBody(cc, id)
		cc.mu.Lock()
		cc.streams[id] = stream
		cc.activeStreams++
		cc.mu.Unlock()
		conn := newClientStreamConn(cc.conn, stream)
		if err := conn.SetWriteDeadline(time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		cc.mu.Lock()
		err := stream.err
		cc.mu.Unlock()
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("stream %d: Close() past the write deadline sent END_STREAM instead of resetting: err=%v", id, err)
		}
	}
}

// stalledTunnelPeer accepts an extended CONNECT, answers 200 and then stops
// reading.
func stalledTunnelPeer(t *testing.T, ln net.Listener) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		setAcceptDeadline(ln, time.Now().Add(5*time.Second))
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.ReadFull(conn, make([]byte, len(clientPreface))); err != nil {
			return
		}
		framer := xhttp2.NewFramer(conn, conn)
		framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
		_ = framer.WriteSettings(
			xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1},
			xhttp2.Setting{ID: xhttp2.SettingEnableConnectProtocol, Val: 1},
		)
		_ = framer.WriteWindowUpdate(0, 1<<31-1-65535)
		var block bytes.Buffer
		encoder := hpack.NewEncoder(&block)
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch frame := frame.(type) {
			case *xhttp2.SettingsFrame:
				if !frame.IsAck() {
					_ = framer.WriteSettingsAck()
				}
			case *xhttp2.MetaHeadersFrame:
				_ = encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				_ = framer.WriteHeaders(xhttp2.HeadersFrameParam{
					StreamID:      frame.StreamID,
					BlockFragment: block.Bytes(),
					EndHeaders:    true,
				})
				<-stop
				return
			}
		}
	}()
}

// A stall WriteByteTimeout ends is the connection's failure, so a tunnel
// Write reports errWriteTimeout, not a deadline error that promises a usable
// stream and not the request paths' ErrTimeout; so does the next Write.
func TestClientStreamConnStallReportsWriteTimeout(t *testing.T) {
	ln := listenPeer(t)
	stalledTunnelPeer(t, ln)
	hc, _ := newPeerClient(t, ln, &ClientConfig{
		EnableExtendedConnect: true,
		WriteByteTimeout:      100 * time.Millisecond,
	})
	var req fasthttp.Request
	var resp fasthttp.Response
	req.Header.SetMethod(fasthttp.MethodConnect)
	req.Header.SetConnectProtocol("websocket")
	req.SetRequestURI("http://" + hc.Addr + "/ws")
	conn, err := hc.OpenStream(&req, &resp)
	if err != nil {
		t.Fatalf("OpenStream() error: %v", err)
	}
	defer conn.Close()
	chunk := make([]byte, 64<<10)
	var first error
	for deadline := time.Now().Add(10 * time.Second); first == nil; {
		if time.Now().After(deadline) {
			t.Fatal("writes against a stalled peer never failed")
		}
		_, first = conn.Write(chunk)
	}
	if !errors.Is(first, errWriteTimeout) || errors.Is(first, os.ErrDeadlineExceeded) {
		t.Fatalf("Write() against the stall = %v, want %v", first, errWriteTimeout)
	}
	if _, second := conn.Write(chunk); !errors.Is(second, errWriteTimeout) {
		t.Fatalf("Write() after the stall = %v, want %v", second, errWriteTimeout)
	}
}

// A tunnel read returns its flow credit through the credit sender, so it
// neither waits for the connection write slot nor outlives its deadline while
// another producer holds the slot; the WINDOW_UPDATE goes out once the slot
// frees.
func TestClientStreamConnReadReturnsCreditWithoutWriteSlot(t *testing.T) {
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	cc := installTestWriter(t, &clientConn{
		conn:    local,
		config:  clientConfig{connectionWindowSize: 65535, streamWindowSize: 65535},
		streams: make(map[uint32]*clientStream),
		pool:    &clientPool{},
	})
	body := newResponseBody(cc, 1)
	if err := body.write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	stream := &clientStream{
		id: 1, conn: cc, isOpenStream: true, requestStarted: true, responseHeader: true,
		responseBody: body, result: make(chan clientResult, 1),
	}
	cc.streams[1] = stream
	cc.activeStreams = 2
	conn := newClientStreamConn(local, stream)

	// Both windows are spent, so the read owes credit on each level.
	cc.writeSem <- struct{}{}
	type readResult struct {
		n   int
		err error
	}
	results := make(chan readResult, 1)
	go func() {
		n, err := conn.Read(make([]byte, 1))
		results <- readResult{n, err}
	}()
	select {
	case got := <-results:
		if got.n != 1 || got.err != nil {
			t.Fatalf("Read() = %d, %v; want the buffered byte", got.n, got.err)
		}
	case <-time.After(2 * time.Second):
		<-cc.writeSem
		t.Fatal("Read() of buffered data waited for the write slot")
	}
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		<-cc.writeSem
		t.Fatalf("Read() past the deadline = %v, want a deadline error", err)
	}

	// The slot frees: the credit reaches the wire.
	<-cc.writeSem
	_ = remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	framer := xhttp2.NewFramer(nil, remote)
	credited := map[uint32]uint32{}
	for len(credited) < 2 {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("reading the client's frames: %v (credited %v)", err, credited)
		}
		if update, ok := frame.(*xhttp2.WindowUpdateFrame); ok {
			credited[update.StreamID] += update.Increment
		}
	}
	if credited[0] != 1 || credited[1] != 1 {
		t.Fatalf("credit returned = %v, want 1 byte on the connection and on stream 1", credited)
	}
}

// Close with a write in flight resets the stream; the response bytes it had
// buffered but not read go back to the connection window first, or later
// streams on the connection starve.
func TestClientStreamConnResettingCloseReturnsConnectionWindow(t *testing.T) {
	const windowSize = 65535
	written := make(chan struct{}, 1)
	finished := make(chan struct{})
	defer close(finished)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		path := string(ctx.Path())
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			_, _ = conn.Write(bytes.Repeat([]byte{'a'}, windowSize))
			if path == "/first" {
				written <- struct{}{}
			}
			<-finished
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true, MaxUploadBufferPerStream: 16})
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{
		Mode:                           PriorKnowledge,
		EnableExtendedConnect:          true,
		MaxResponseBufferPerConnection: windowSize,
		MaxResponseBufferPerStream:     windowSize,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	open := func(path string) fasthttp.StreamConn {
		t.Helper()
		var req fasthttp.Request
		var resp fasthttp.Response
		req.Header.SetMethod(fasthttp.MethodConnect)
		req.Header.SetConnectProtocol("websocket")
		req.SetRequestURI(testServer.URL(path))
		conn, err := hc.OpenStream(&req, &resp)
		if err != nil {
			t.Fatalf("OpenStream(%s) error: %v", path, err)
		}
		return conn
	}

	first := open("/first")
	<-written
	time.Sleep(100 * time.Millisecond) // the read loop buffers the whole window
	writeDone := make(chan error, 1)
	go func() {
		_, err := first.Write(make([]byte, 64)) // stalls on the server's 16-byte window
		writeDone <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := <-writeDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write() interrupted by Close() = %v, want %v", err, net.ErrClosed)
	}

	second := open("/second")
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, windowSize)
	if n, err := io.ReadFull(second, buf); err != nil {
		t.Fatalf("second stream read %d of %d bytes: %v", n, windowSize, err)
	}
}

// A Read interrupted by its cut reports the interruption even when the body
// completed meanwhile; buffered data still returns first.
func TestResponseBodyReadPrefersCutOverCompletion(t *testing.T) {
	cc := installTestWriter(t, &clientConn{streams: make(map[uint32]*clientStream)})
	body := newResponseBody(cc, 1)
	if err := body.write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	body.complete()
	if n, err := body.readUntil(make([]byte, 1), closedStreamDone); n != 1 || err != nil {
		t.Fatalf("readUntil() with buffered data = %d, %v; want 1, nil", n, err)
	}
	if _, err := body.readUntil(make([]byte, 1), closedStreamDone); !errors.Is(err, errInterrupted) {
		t.Fatalf("readUntil() under a closed cut = %v, want %v", err, errInterrupted)
	}
	if _, err := body.readUntil(make([]byte, 1), nil); !errors.Is(err, io.EOF) {
		t.Fatalf("readUntil() without a cut = %v, want EOF", err)
	}
}

// acceptedTunnelEnds opens one tunnel and returns both StreamConn ends; the
// server's handler holds the stream until the test ends.
func acceptedTunnelEnds(t *testing.T) (client, server fasthttp.StreamConn) {
	t.Helper()
	ends := make(chan fasthttp.StreamConn, 1)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			ends <- conn
			<-hold
		})
	}}, ServerConfig{EnableExtendedConnect: true})
	client = openTestStreamConn(t, testServer, nil)
	select {
	case server = <-ends:
	case <-time.After(5 * time.Second):
		t.Fatal("the server didn't accept the stream")
	}
	return client, server
}

// net.Conn: a zero-length Read returns (0, nil) at once on an open stream, as
// a TCP connection's does, and the close error once the StreamConn is closed.
func TestStreamConnZeroLengthReadReturnsAtOnce(t *testing.T) {
	client, server := acceptedTunnelEnds(t)
	for _, end := range []struct {
		name string
		conn fasthttp.StreamConn
	}{{"client", client}, {"server", server}} {
		t.Run(end.name, func(t *testing.T) {
			if err := end.conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if n, err := end.conn.Read(nil); n != 0 || err != nil {
				t.Fatalf("Read(nil) = %d, %v after %v; want 0, nil", n, err, time.Since(start))
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("Read(nil) took %v", elapsed)
			}
			if err := end.conn.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := end.conn.Read(nil); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Read(nil) after Close = %v, want net.ErrClosed", err)
			}
		})
	}
}

// A zero-length Read reports a peer reset with the error the body's readers
// report, and the data buffered before the reset is still read first.
func TestStreamConnZeroLengthReadReportsPeerReset(t *testing.T) {
	t.Run("client", func(t *testing.T) {
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{EnableExtendedConnect: true})
		var peer *clientPeer
		conn, streamID := openTunnel(t, hc, ln, &peer)
		defer conn.Close()
		peer.writeData(streamID, false, []byte("hi"))
		peer.write(func(framer *xhttp2.Framer) error {
			return framer.WriteRSTStream(streamID, xhttp2.ErrCodeCancel)
		})
		var err error
		waitUntil(t, "the reset reaches the stream", func() bool {
			_, err = conn.Read(nil)
			return err != nil
		})
		streamErr, ok := errors.AsType[*StreamError](err)
		if !ok || streamErr.ErrCode != uint32(xhttp2.ErrCodeCancel) {
			t.Fatalf("Read(nil) after the reset = %v, want the peer's CANCEL", err)
		}
		buf := make([]byte, 2)
		if n, err := conn.Read(buf); n != 2 || err != nil || string(buf) != "hi" {
			t.Fatalf("Read() of the data before the reset = %d, %v, %q", n, err, buf[:n])
		}
		if _, err := conn.Read(buf); !errors.Is(err, streamErr) {
			t.Fatalf("Read() after the data = %v, want %v", err, streamErr)
		}
	})
	t.Run("server", func(t *testing.T) {
		ends := make(chan fasthttp.StreamConn, 1)
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				ends <- conn
				<-hold
			})
		}}, ServerConfig{EnableExtendedConnect: true})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		openRawTunnel(peer, 1)
		var conn fasthttp.StreamConn
		select {
		case conn = <-ends:
		case <-time.After(5 * time.Second):
			t.Fatal("the server didn't accept the stream")
		}
		if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
		var err error
		waitUntil(t, "the reset reaches the stream", func() bool {
			_, err = conn.Read(nil)
			return err != nil
		})
		if !errors.Is(err, errStreamClosed) {
			t.Fatalf("Read(nil) after the reset = %v, want the peer reset", err)
		}
		if _, readErr := conn.Read(make([]byte, 1)); !errors.Is(readErr, err) {
			t.Fatalf("Read() after the reset = %v, want %v", readErr, err)
		}
	})
}

// A peer's reset reaches a client StreamConn as a *StreamError that matches
// errStreamClosed, as the server's StreamConn reports its peer's reset; a
// read side the peer ended stays at EOF, and REFUSED_STREAM keeps matching
// ErrRefusedStream.
func TestStreamConnPeerResetMatchesClosedStream(t *testing.T) {
	refused := &StreamError{ErrCode: uint32(xhttp2.ErrCodeRefusedStream)}
	if !errors.Is(refused, ErrRefusedStream) || !errors.Is(refused, errStreamClosed) {
		t.Fatalf("REFUSED_STREAM matches ErrRefusedStream=%v errStreamClosed=%v, want both",
			errors.Is(refused, ErrRefusedStream), errors.Is(refused, errStreamClosed))
	}
	canceled := &StreamError{ErrCode: uint32(xhttp2.ErrCodeCancel)}
	if errors.Is(canceled, ErrRefusedStream) {
		t.Fatal("CANCEL matches ErrRefusedStream")
	}
	t.Run("client", func(t *testing.T) {
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{EnableExtendedConnect: true})
		var peer *clientPeer
		conn, streamID := openTunnel(t, hc, ln, &peer)
		defer conn.Close()
		peer.writeData(streamID, true, nil)
		peer.write(func(framer *xhttp2.Framer) error { return framer.WriteRSTStream(streamID, xhttp2.ErrCodeNo) })
		buf := make([]byte, 1)
		if n, err := conn.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("Read() after the peer's END_STREAM = %d, %v, want EOF", n, err)
		}
		var err error
		waitUntil(t, "the reset reaches the stream", func() bool {
			_, err = conn.Write([]byte("x"))
			return err != nil
		})
		if _, ok := errors.AsType[*StreamError](err); !ok || !errors.Is(err, errStreamClosed) {
			t.Fatalf("Write() after the reset = %v, want a *StreamError matching errStreamClosed", err)
		}
		if _, err := conn.Read(buf); !errors.Is(err, io.EOF) {
			t.Fatalf("Read() after the reset = %v, want EOF", err)
		}
	})
	t.Run("server", func(t *testing.T) {
		ends := make(chan fasthttp.StreamConn, 1)
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				ends <- conn
				<-hold
			})
		}}, ServerConfig{EnableExtendedConnect: true})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		openRawTunnel(peer, 1)
		var conn fasthttp.StreamConn
		select {
		case conn = <-ends:
		case <-time.After(5 * time.Second):
			t.Fatal("the server didn't accept the stream")
		}
		if err := peer.framer.WriteData(1, true, nil); err != nil {
			t.Fatal(err)
		}
		if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeNo); err != nil {
			t.Fatal(err)
		}
		var err error
		waitUntil(t, "the reset reaches the stream", func() bool {
			_, err = conn.Write([]byte("x"))
			return err != nil
		})
		if !errors.Is(err, errStreamClosed) {
			t.Fatalf("Write() after the reset = %v, want the peer reset", err)
		}
	})
}
