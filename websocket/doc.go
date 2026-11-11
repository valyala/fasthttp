// Package websocket implements the WebSocket protocol (RFC 6455) on top of
// fasthttp.
//
// Servers call Accept from a fasthttp request handler; applications served
// through fasthttpadaptor call it from a fasthttp handler, not from the
// adapted net/http one. Clients call Dial.
//
// Conn methods take a context for cancellation and deadlines. A read or write
// that is cancelled or times out closes the connection, since the frame stream
// cannot be resumed; cancelling or timing out while waiting for another call
// to finish does not. ReadTimeout and WriteTimeout in the options bound each
// read and frame write without wrapping each context; see AcceptOptions.
//
// Once a connection is closed, reads and writes return the error that closed
// it. A closing handshake, a protocol violation by the peer or a transport
// that failed without a Close frame yield a CloseError value, the last with
// StatusAbnormalClosure: inspect it with CloseStatus, or with
//
//	var ce CloseError
//	if errors.As(err, &ce) { ... }
//
// A cancelled or expired context yields its error, a ReadTimeout or
// WriteTimeout a timeout error, and CloseNow or a write after the Close frame
// went out net.ErrClosed.
//
// Compression with permessage-deflate (RFC 7692) is off unless CompressionMode
// in the options enables it; MaxMessageSize then bounds messages before and
// after decompression.
package websocket
