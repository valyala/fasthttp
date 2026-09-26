package http2

import (
	"crypto/tls"
	"errors"
	"net"
)

var errInadequateTLS = errors.New("http2: TLS parameters don't satisfy RFC 9113")

type tlsConnectionStater interface {
	ConnectionState() tls.ConnectionState
}

func validateTLSConnection(conn net.Conn) error {
	tlsConn, ok := conn.(tlsConnectionStater)
	if !ok {
		return nil
	}
	state := tlsConn.ConnectionState()
	if state.Version < tls.VersionTLS12 {
		return errInadequateTLS
	}
	if state.Version >= tls.VersionTLS13 {
		return nil
	}
	switch state.CipherSuite {
	case tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256:
		return nil
	default:
		return errInadequateTLS
	}
}
