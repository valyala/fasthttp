package fasthttp

import "testing"

func TestStatusMessage(t *testing.T) {
	statusMessages := map[int]string{
		StatusContinue:           "Continue",
		StatusSwitchingProtocols: "SwitchingProtocols",

		StatusOK:                   "OK",
		StatusCreated:              "Created",
		StatusAccepted:             "Accepted",
		StatusNonAuthoritativeInfo: "Non-Authoritative Info",
		StatusNoContent:            "No Content",
		StatusResetContent:         "Reset Content",
		StatusPartialContent:       "Partial Content",

		StatusMultipleChoices:   "Multiple Choices",
		StatusMovedPermanently:  "Moved Permanently",
		StatusFound:             "Found",
		StatusSeeOther:          "See Other",
		StatusNotModified:       "Not Modified",
		StatusUseProxy:          "Use Proxy",
		StatusTemporaryRedirect: "Temporary Redirect",

		StatusBadRequest:                   "Bad Request",
		StatusUnauthorized:                 "Unauthorized",
		StatusPaymentRequired:              "Payment Required",
		StatusForbidden:                    "Forbidden",
		StatusNotFound:                     "Not Found",
		StatusMethodNotAllowed:             "Method Not Allowed",
		StatusNotAcceptable:                "Not Acceptable",
		StatusProxyAuthRequired:            "Proxy Auth Required",
		StatusRequestTimeout:               "Request Timeout",
		StatusConflict:                     "Conflict",
		StatusGone:                         "Gone",
		StatusLengthRequired:               "Length Required",
		StatusPreconditionFailed:           "Precondition Failed",
		StatusRequestEntityTooLarge:        "Request Entity Too Large",
		StatusRequestURITooLong:            "Request URI Too Long",
		StatusUnsupportedMediaType:         "Unsupported Media Type",
		StatusRequestedRangeNotSatisfiable: "Requested Range Not Satisfiable",
		StatusExpectationFailed:            "Expectation Failed",
		StatusTeapot:                       "Teapot",
		StatusPreconditionRequired:         "Precondition Required",
		StatusTooManyRequests:              "Too Many Requests",
		StatusRequestHeaderFieldsTooLarge:  "Request HeaderFields Too Large",

		StatusInternalServerError:           "Internal Server Error",
		StatusNotImplemented:                "Not Implemented",
		StatusBadGateway:                    "Bad Gateway",
		StatusServiceUnavailable:            "Service Unavailable",
		StatusGatewayTimeout:                "Gateway Timeout",
		StatusHTTPVersionNotSupported:       "HTTP Version Not Supported",
		StatusNetworkAuthenticationRequired: "Network Authentication Required",
	}

	for code, want := range statusMessages {
		got := StatusMessage(code)
		if got != want {
			t.Errorf("Unexpected text for code=%d, got %s; want %s", code, got, want)
		}
	}
}
