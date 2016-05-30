package fasthttp

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// HTTP status codes were stolen from net/http.
const (
	StatusContinue           = 100
	StatusSwitchingProtocols = 101

	StatusOK                   = 200
	StatusCreated              = 201
	StatusAccepted             = 202
	StatusNonAuthoritativeInfo = 203
	StatusNoContent            = 204
	StatusResetContent         = 205
	StatusPartialContent       = 206

	StatusMultipleChoices   = 300
	StatusMovedPermanently  = 301
	StatusFound             = 302
	StatusSeeOther          = 303
	StatusNotModified       = 304
	StatusUseProxy          = 305
	StatusTemporaryRedirect = 307

	StatusBadRequest                   = 400
	StatusUnauthorized                 = 401
	StatusPaymentRequired              = 402
	StatusForbidden                    = 403
	StatusNotFound                     = 404
	StatusMethodNotAllowed             = 405
	StatusNotAcceptable                = 406
	StatusProxyAuthRequired            = 407
	StatusRequestTimeout               = 408
	StatusConflict                     = 409
	StatusGone                         = 410
	StatusLengthRequired               = 411
	StatusPreconditionFailed           = 412
	StatusRequestEntityTooLarge        = 413
	StatusRequestURITooLong            = 414
	StatusUnsupportedMediaType         = 415
	StatusRequestedRangeNotSatisfiable = 416
	StatusExpectationFailed            = 417
	StatusTeapot                       = 418
	StatusPreconditionRequired         = 428
	StatusTooManyRequests              = 429
	StatusRequestHeaderFieldsTooLarge  = 431

	StatusInternalServerError           = 500
	StatusNotImplemented                = 501
	StatusBadGateway                    = 502
	StatusServiceUnavailable            = 503
	StatusGatewayTimeout                = 504
	StatusHTTPVersionNotSupported       = 505
	StatusNetworkAuthenticationRequired = 511
)

var statusLines atomic.Value

// StatusMessage returns HTTP status message for the given status code.
func StatusMessage(statusCode int) string {
	return http.StatusText(statusCode)
}

func init() {
	statusLines.Store(make(map[int][]byte))
}

func statusLine(statusCode int) []byte {
	m := statusLines.Load().(map[int][]byte)
	h := m[statusCode]
	if h != nil {
		return h
	}

	statusText := StatusMessage(statusCode)

	h = []byte(fmt.Sprintf("HTTP/1.1 %d %s\r\n", statusCode, statusText))
	newM := make(map[int][]byte, len(m)+1)
	for k, v := range m {
		newM[k] = v
	}
	newM[statusCode] = h
	statusLines.Store(newM)
	return h
}
