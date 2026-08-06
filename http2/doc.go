// Package http2 provides experimental native HTTP/2 support for fasthttp.
//
// HTTP/2 is opt-in. Use ConfigureServer for ALPN and same-port cleartext
// prior-knowledge dispatch, or ServeConn for a dedicated prior-knowledge
// connection. ConfigureHostClient and ConfigureClient install the multiplexed
// client transport while retaining the built-in HTTP/1 fallback. TLS clients
// prefer HTTP/2 unless RequireHTTP2 is selected; cleartext HTTP/2 requires
// PriorKnowledge. The package doesn't implement the obsolete h2c Upgrade path.
//
// Server push and extended CONNECT are disabled by default. Extended CONNECT
// is exposed as a request-scoped fasthttp.StreamConn. Physical connection
// hijacking is unavailable to multiplexed requests; RequestCtx.TryHijack
// returns fasthttp.ErrHijackNotSupported.
package http2
