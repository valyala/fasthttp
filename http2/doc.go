// Package http2 provides experimental native HTTP/2 support for fasthttp.
//
// HTTP/2 is opt-in. Use ConfigureServer for ALPN and same-port cleartext
// prior-knowledge dispatch, or ServeConn for a dedicated prior-knowledge
// connection. The package doesn't implement the obsolete h2c Upgrade path.
//
// Experimental: the package API may change before it has shipped in two
// fasthttp minor releases.
package http2
