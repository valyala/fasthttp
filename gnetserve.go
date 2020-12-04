package fasthttp

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/panjf2000/gnet"
)

//GnetConn - Implements the net.Conn interface to allow adapting Gnet to the serveConn method
type GnetConn struct {
	gnetConn    gnet.Conn
	readBuffer  *bytes.Buffer
	writeBuffer *bytes.Buffer
	closed      bool
}

// Read reads data from the connection.
// Read can be made to time out and return an error after a fixed
// time limit; see SetDeadline and SetReadDeadline.
func (gc *GnetConn) Read(b []byte) (n int, err error) {
	return gc.readBuffer.Read(b)
}

// Write writes data to the connection.
// Write can be made to time out and return an error after a fixed
// time limit; see SetDeadline and SetWriteDeadline.
func (gc *GnetConn) Write(b []byte) (n int, err error) {
	return gc.writeBuffer.Write(b)
}

// Close closes the connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (gc *GnetConn) Close() error {
	gc.closed = true
	return nil
}

// LocalAddr returns the local network address.
func (gc *GnetConn) LocalAddr() net.Addr {
	return gc.gnetConn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (gc *GnetConn) RemoteAddr() net.Addr {
	return gc.gnetConn.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
//
// A deadline is an absolute time after which I/O operations
// fail instead of blocking. The deadline applies to all future
// and pending I/O, not just the immediately following call to
// Read or Write. After a deadline has been exceeded, the
// connection can be refreshed by setting a deadline in the future.
//
// If the deadline is exceeded a call to Read or Write or to other
// I/O methods will return an error that wraps os.ErrDeadlineExceeded.
// This can be tested using errors.Is(err, os.ErrDeadlineExceeded).
// The error's Timeout method will return true, but note that there
// are other possible errors for which the Timeout method will
// return true even if the deadline has not been exceeded.
//
// An idle timeout can be implemented by repeatedly extending
// the deadline after successful Read or Write calls.
//
// A zero value for t means I/O operations will not time out.
func (gc *GnetConn) SetDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
// A zero value for t means Read will not time out.
func (gc *GnetConn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls
// and any currently-blocked Write call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
func (gc *GnetConn) SetWriteDeadline(t time.Time) error {
	return nil
}

//Reinit Resets and initializes the buffers with a new connection
func (gc *GnetConn) Reinit(b []byte, c gnet.Conn) {
	gc.closed = false
	gc.gnetConn = c
	gc.writeBuffer.Reset()
	gc.readBuffer.Reset()
	_, _ = gc.readBuffer.Write(b)
}

// GnetHTTP -
type GnetHTTP struct {
	*gnet.EventServer
	fasthttpserver *Server
}

// OnOpened fires when a new connection has been opened.
// The parameter:c has information about the connection such as it's local and remote address.
// Parameter:out is the return value which is going to be sent back to the client.
func (es *GnetHTTP) OnOpened(c gnet.Conn) (out []byte, action gnet.Action) {
	return
}

// OnClosed fires when a connection has been closed.
// The parameter:err is the last known connection error.
func (es *GnetHTTP) OnClosed(c gnet.Conn, err error) (action gnet.Action) {
	return
}

// PreWrite fires just before any data is written to any client socket, this event function is usually used to
// put some code of logging/counting/reporting or any prepositive operations before writing data to client.
func (es *GnetHTTP) PreWrite() {
}

type httpCodec struct {
	fasthttpserver *Server
	gconnPool      sync.Pool
}

func (hc *httpCodec) Encode(c gnet.Conn, buf []byte) (out []byte, err error) {
	return buf, nil
}

func (hc *httpCodec) Decode(c gnet.Conn) (out []byte, err error) {
	buf := c.Read()
	if len(buf) == 0 {
		return
	}

	//Re-use buffers...
	gconn := hc.gconnPool.Get().(*GnetConn)
	gconn.Reinit(buf, c)

	//Bypasses the workpoll based implementation...
	err = hc.fasthttpserver.serveConn(gconn)

	//Reuse buffer...
	hc.gconnPool.Put(gconn)

	if err == nil {
		c.ResetBuffer()
		return gconn.writeBuffer.Bytes(), err
	}

	if err != errHijacked {
		c.ResetBuffer()
		return gconn.writeBuffer.Bytes(), err
	}

	log.Println("request not ready, yet (Partial request?)")

	return nil, err
}

//OnInitComplete -
func (es *GnetHTTP) OnInitComplete(srv gnet.Server) (action gnet.Action) {
	log.Printf("HTTP server is listening on %s (multi-cores: %t, loops: %d)\n",
		srv.Addr.String(), srv.Multicore, srv.NumEventLoop)
	return
}

//React - Forwards the result of httpcodec Decode
func (es *GnetHTTP) React(frame []byte, c gnet.Conn) (out []byte, action gnet.Action) {
	out = frame
	return
}

//ListenAndServeGnet uses gnet for non-blocking event based connections...
func ListenAndServeGnet(addr string, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ListenAndServeGnet(addr)
}

//ListenAndServeGnet uses gnet for non-blocking event based connections...
func (s *Server) ListenAndServeGnet(addr string) error {

	hc := &httpCodec{
		fasthttpserver: s,
		gconnPool: sync.Pool{
			New: func() interface{} {
				return &GnetConn{
					readBuffer:  &bytes.Buffer{},
					writeBuffer: &bytes.Buffer{},
				}
			},
		},
	}

	server := &GnetHTTP{fasthttpserver: s}
	err := gnet.Serve(server, fmt.Sprintf("tcp://%v", addr), gnet.WithMulticore(true), gnet.WithCodec(hc))
	if err != nil {
		log.Println("Error gnet serve", err)
	}
	return err
}

//StopServeGnet ... stops gnet server
func StopServeGnet(addr string) {
	err := gnet.Stop(context.Background(), fmt.Sprintf("tcp://%v", addr))
	if err != nil {
		log.Println("Error StopServeGnet", err)
	}
}
