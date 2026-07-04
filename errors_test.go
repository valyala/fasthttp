package fasthttp

import "testing"

func TestExportedErrorStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "ErrAlreadyServing",
			err:  ErrAlreadyServing,
			want: "fasthttp: server is already serving connections",
		},
		{
			name: "ErrMissingFile",
			err:  ErrMissingFile,
			want: "fasthttp: there is no uploaded file associated with the given key",
		},
		{
			name: "ErrPerIPConnLimit",
			err:  ErrPerIPConnLimit,
			want: "fasthttp: too many connections per ip",
		},
		{
			name: "ErrConcurrencyLimit",
			err:  ErrConcurrencyLimit,
			want: "fasthttp: cannot serve the connection because server.concurrency concurrent connections are served",
		},
		{
			name: "ErrBadTrailer",
			err:  ErrBadTrailer,
			want: "fasthttp: contain forbidden trailer",
		},
		{
			name: "ErrReadingResponseHeaders",
			err:  ErrReadingResponseHeaders,
			want: "fasthttp: error when reading response headers",
		},
		{
			name: "ErrReadingResponseTrailer",
			err:  ErrReadingResponseTrailer,
			want: "fasthttp: error when reading response trailer",
		},
		{
			name: "ErrResponseFirstLineMissingSpace",
			err:  ErrResponseFirstLineMissingSpace,
			want: "fasthttp: cannot find whitespace in the first line of response",
		},
		{
			name: "ErrUnexpectedStatusCodeChar",
			err:  ErrUnexpectedStatusCodeChar,
			want: "fasthttp: unexpected char at the end of status code",
		},
		{
			name: "ErrMissingRequestMethod",
			err:  ErrMissingRequestMethod,
			want: "fasthttp: cannot find http request method",
		},
		{
			name: "ErrUnsupportedRequestMethod",
			err:  ErrUnsupportedRequestMethod,
			want: "fasthttp: unsupported http request method",
		},
		{
			name: "ErrExtraWhitespaceInRequestLine",
			err:  ErrExtraWhitespaceInRequestLine,
			want: "fasthttp: extra whitespace in request line",
		},
		{
			name: "ErrEmptyRequestURI",
			err:  ErrEmptyRequestURI,
			want: "fasthttp: requesturi cannot be empty",
		},
		{
			name: "ErrDuplicateContentLength",
			err:  ErrDuplicateContentLength,
			want: "fasthttp: duplicate content-length header",
		},
		{
			name: "ErrUnsupportedTransferEncoding",
			err:  ErrUnsupportedTransferEncoding,
			want: "fasthttp: unsupported transfer-encoding",
		},
		{
			name: "ErrNonNumericChars",
			err:  ErrNonNumericChars,
			want: "fasthttp: non-numeric chars found",
		},
		{
			name: "ErrNeedMore",
			err:  ErrNeedMore,
			want: "fasthttp: need more data: cannot find trailing lf",
		},
		{
			name: "ErrSmallReadBuffer",
			err:  ErrSmallReadBuffer,
			want: "fasthttp: small read buffer. increase readbuffersize",
		},
		{
			name: "ErrNoArgValue",
			err:  ErrNoArgValue,
			want: "fasthttp: no args value for the given key",
		},
		{
			name: "ErrorInvalidURI",
			err:  ErrorInvalidURI,
			want: "fasthttp: invalid uri",
		},
		{
			name: "ErrDialTimeout",
			err:  ErrDialTimeout,
			want: "fasthttp: dialing to the given tcp address timed out",
		},
		{
			name: "ErrContentEncodingUnsupported",
			err:  ErrContentEncodingUnsupported,
			want: "fasthttp: unsupported content-encoding",
		},
		{
			name: "ErrNoMultipartForm",
			err:  ErrNoMultipartForm,
			want: "fasthttp: request content-type has bad boundary or is not multipart/form-data",
		},
		{
			name: "ErrGetOnly",
			err:  ErrGetOnly,
			want: "fasthttp: non-get request received",
		},
		{
			name: "ErrBodyTooLarge",
			err:  ErrBodyTooLarge,
			want: "fasthttp: body size exceeds the given limit",
		},
		{
			name: "ErrNoCookies",
			err:  ErrNoCookies,
			want: "fasthttp: no cookies found",
		},
		{
			name: "ErrInvalidCookieValue",
			err:  ErrInvalidCookieValue,
			want: "fasthttp: invalid cookie value",
		},
		{
			name: "ErrNoAvailableClients",
			err:  ErrNoAvailableClients,
			want: "fasthttp: no available clients",
		},
		{
			name: "ErrMissingLocation",
			err:  ErrMissingLocation,
			want: "fasthttp: missing location header for http redirect",
		},
		{
			name: "ErrTooManyRedirects",
			err:  ErrTooManyRedirects,
			want: "fasthttp: too many redirects detected when doing the request",
		},
		{
			name: "ErrHostClientRedirectToDifferentScheme",
			err:  ErrHostClientRedirectToDifferentScheme,
			want: "fasthttp: hostclient can't follow redirects to a different protocol, please use client instead",
		},
		{
			name: "ErrNoFreeConns",
			err:  ErrNoFreeConns,
			want: "fasthttp: no free connections available to host",
		},
		{
			name: "ErrConnectionClosed",
			err:  ErrConnectionClosed,
			want: "fasthttp: the server closed connection before returning the first response byte. make sure the server returns 'connection: close' response header before closing the connection",
		},
		{
			name: "ErrConnPoolStrategyNotImpl",
			err:  ErrConnPoolStrategyNotImpl,
			want: "fasthttp: connection pool strategy is not implement",
		},
		{
			name: "ErrTimeout",
			err:  ErrTimeout,
			want: "fasthttp: timeout",
		},
		{
			name: "ErrTLSHandshakeTimeout",
			err:  ErrTLSHandshakeTimeout,
			want: "fasthttp: tls handshake timed out",
		},
		{
			name: "ErrPipelineOverflow",
			err:  ErrPipelineOverflow,
			want: "fasthttp: pipelined requests' queue has been overflowed. increase maxconns and/or maxpendingrequests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("unexpected error string:\ngot  %q\nwant %q", got, tt.want)
			}
		})
	}
}
