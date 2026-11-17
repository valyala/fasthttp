// Package http2 provides native HTTP/2 support for fasthttp.
//
// HTTP/2 is opt-in. Use ConfigureServer for ALPN, same-port cleartext
// prior-knowledge dispatch and the h2c Upgrade handshake, or ServeConn for a
// dedicated prior-knowledge connection.
//
// Server push and extended CONNECT are disabled by default. Extended CONNECT
// is exposed as a request-scoped fasthttp.StreamConn. Physical connection
// hijacking is unavailable to multiplexed requests; RequestCtx.TryHijack
// returns fasthttp.ErrHijackNotSupported.
package http2
