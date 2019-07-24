package fasthttp

import "net"

// ServerTrace is a set of hooks to run at various stages of an incoming HTTP
// request. Any particular hook may be nil. Functions may be called
// concurrently from different goroutines and some may be called after the
// request has completed or failed.
type ServerTrace struct {
	// GotConn is called whenever a new connection to a requestor has been
	// established by the <Serve> function. The passed in <conn> is owned by the
	// server and should not be read, writeen, or closed by users of
	// <ServerTrace>.
	GotConn func(conn net.Conn)

	// ClosedConn is called after a connection to a remove peer has been closed
	ClosedConn func(conn net.Conn)

	// ActivatedConn is called when a connection has been activated and 1 or more
	// bytes of the request has been read.
	ActivatedConn func(conn net.Conn)

	// IdledConn is called when a request of a connection has been processed and
	// the response has been sent. The connection is now entering a keep alive
	// state until it is activated again by the next request it may send.
	IdledConn func(conn net.Conn)

	// HijackedConn is called after a connection has been hijacked by the request
	// handler. Note that <ClosedConn> will not be called as connection handling
	// has been passed off to the hijacker.
	HijackedConn func(conn net.Conn)

	// GotRequest is called when a new request has been received before the
	// configured handler is called.
	GotRequest func(ctx *RequestCtx)

	// AcquiredContext is called when the server acquired a context for the
	// connection before the first request is received.
	AcquiredContext func(ctx *RequestCtx)

	// WroteResponse is called after the response for a given request has been
	// sent. <n> is the number of bytes that have been transferred and <err> is
	// any error that may have been occurred while writing the response.
	WroteResponse func(ctx *RequestCtx, n int64, err error)
}
