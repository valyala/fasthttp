package http2

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpguts"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	errInvalidRequestHeaders  = errors.New("http2: invalid request headers")
	errRequestBodyTooLarge    = errors.New("http2: request body too large")
	errResponseHeaderTooLarge = errors.New("http2: response header list too large")
)

const (
	pseudoMethod = 1 << iota
	pseudoScheme
	pseudoAuthority
	pseudoPath
	pseudoProtocol
)

func populateRequest(
	ctx *fasthttp.RequestCtx,
	server *fasthttp.Server,
	fields []hpack.HeaderField,
	enableExtendedConnect bool,
) (int64, error) {
	if server.DisableHeaderNamesNormalizing {
		ctx.Request.Header.DisableNormalizing()
	}

	var method string
	var scheme string
	var authority string
	var path string
	var connectProtocol string
	var host string
	var contentLength int64 = -1
	var seenPseudo uint8
	cookies := make([]string, 0, 1)
	for _, field := range fields {
		if field.IsPseudo() {
			var bit uint8
			switch field.Name {
			case ":method":
				bit, method = pseudoMethod, field.Value
			case ":scheme":
				bit, scheme = pseudoScheme, field.Value
			case ":authority":
				bit, authority = pseudoAuthority, field.Value
			case ":path":
				bit, path = pseudoPath, field.Value
			case ":protocol":
				bit, connectProtocol = pseudoProtocol, field.Value
			default:
				return -1, fmt.Errorf("%w: unknown pseudo-header %q", errInvalidRequestHeaders, field.Name)
			}
			if seenPseudo&bit != 0 {
				return -1, fmt.Errorf("%w: duplicate pseudo-header %q", errInvalidRequestHeaders, field.Name)
			}
			seenPseudo |= bit
			continue
		}

		name := field.Name
		value := field.Value
		if isConnectionSpecificHeader(name) {
			return -1, fmt.Errorf("%w: connection-specific header %q", errInvalidRequestHeaders, name)
		}
		if name == "te" && !strings.EqualFold(strings.TrimSpace(value), "trailers") {
			return -1, fmt.Errorf("%w: invalid te header", errInvalidRequestHeaders)
		}
		if name == "content-length" {
			parsed, err := parseHTTP2ContentLength(value)
			if err != nil {
				return -1, err
			}
			if contentLength >= 0 && contentLength != parsed {
				return -1, fmt.Errorf("%w: conflicting content-length", errInvalidRequestHeaders)
			}
			contentLength = parsed
		}
		if name == "host" {
			host = value
			continue
		}
		if name == "cookie" {
			cookies = append(cookies, value)
			continue
		}
		ctx.Request.Header.Add(name, value)
	}

	if method == "" {
		return -1, fmt.Errorf("%w: missing :method", errInvalidRequestHeaders)
	}
	isConnect := method == fasthttp.MethodConnect
	switch {
	case connectProtocol != "":
		if !enableExtendedConnect {
			return -1, fmt.Errorf("%w: extended connect is disabled", errInvalidRequestHeaders)
		}
		if !isConnect || scheme == "" || authority == "" || path == "" {
			return -1, fmt.Errorf("%w: malformed extended connect", errInvalidRequestHeaders)
		}
	case isConnect:
		if authority == "" || scheme != "" || path != "" {
			return -1, fmt.Errorf("%w: malformed connect", errInvalidRequestHeaders)
		}
	default:
		if scheme == "" || path == "" {
			return -1, fmt.Errorf("%w: missing request pseudo-header", errInvalidRequestHeaders)
		}
	}
	if authority == "" {
		authority = host
	}
	if authority == "" {
		return -1, fmt.Errorf("%w: missing authority", errInvalidRequestHeaders)
	}
	if !httpguts.ValidHostHeader(authority) {
		return -1, fmt.Errorf("%w: invalid authority", errInvalidRequestHeaders)
	}
	if host != "" && host != authority {
		if !strings.EqualFold(host, authority) {
			return -1, fmt.Errorf("%w: host and authority differ", errInvalidRequestHeaders)
		}
	}
	if path != "" && path != "*" && path[0] != '/' {
		return -1, fmt.Errorf("%w: invalid :path", errInvalidRequestHeaders)
	}
	if path == "*" && method != fasthttp.MethodOptions {
		return -1, fmt.Errorf("%w: asterisk :path requires OPTIONS", errInvalidRequestHeaders)
	}
	if scheme != "" && !validScheme(scheme) {
		return -1, fmt.Errorf("%w: invalid :scheme", errInvalidRequestHeaders)
	}

	ctx.Request.Header.SetMethod(method)
	ctx.Request.Header.SetProtocol("HTTP/2")
	ctx.Request.Header.SetHost(authority)
	if path != "" {
		ctx.Request.Header.SetRequestURI(path)
	}
	if connectProtocol != "" {
		ctx.Request.Header.SetConnectProtocol(connectProtocol)
	}
	if len(cookies) != 0 {
		ctx.Request.Header.Set(fasthttp.HeaderCookie, strings.Join(cookies, "; "))
	}
	return contentLength, nil
}

func validScheme(scheme string) bool {
	if scheme == "" {
		return false
	}
	first := scheme[0]
	if (first < 'A' || first > 'Z') && (first < 'a' || first > 'z') {
		return false
	}
	for i := 1; i < len(scheme); i++ {
		value := scheme[i]
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
			value >= '0' && value <= '9' || value == '+' || value == '-' || value == '.' {
			continue
		}
		return false
	}
	return true
}

func parseHTTP2ContentLength(value string) (int64, error) {
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, fmt.Errorf("%w: invalid content-length", errInvalidRequestHeaders)
		}
	}
	length, err := strconv.ParseInt(value, 10, 64)
	if err != nil || uint64(length) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%w: invalid content-length", errInvalidRequestHeaders)
	}
	return length, nil
}

func encodeTrailerHeaders(
	encoder *hpack.Encoder,
	buffer *bytes.Buffer,
	stringsCache *headerStringCache,
	header *fasthttp.ResponseHeader,
	maxHeaderListSize uint64,
) ([]byte, error) {
	headerSize := uint64(0)
	for _, key := range header.PeekTrailerKeys() {
		name := stringsCache.name(key)
		if name == "te" || isConnectionSpecificHeader(name) {
			return nil, fmt.Errorf("http2: invalid response trailer %q", name)
		}
		for _, value := range header.PeekAll(string(key)) {
			headerSize += uint64(len(name) + len(value) + 32)
			if maxHeaderListSize != 0 && headerSize > maxHeaderListSize {
				return nil, errResponseHeaderTooLarge
			}
		}
	}
	buffer.Reset()
	for _, key := range header.PeekTrailerKeys() {
		name := stringsCache.name(key)
		values := header.PeekAll(string(key))
		for _, value := range values {
			sensitive := isSensitiveHeader(name)
			if err := encoder.WriteField(hpack.HeaderField{
				Name:      name,
				Value:     stringsCache.value(value, sensitive),
				Sensitive: sensitive,
			}); err != nil {
				return nil, err
			}
		}
	}
	return buffer.Bytes(), nil
}

func encodeResponseHeaders(
	encoder *hpack.Encoder,
	buffer *bytes.Buffer,
	stringsCache *headerStringCache,
	server *fasthttp.Server,
	response *fasthttp.Response,
	maxHeaderListSize uint64,
	serverDate []byte,
	scratch *[]hpack.HeaderField,
) ([]byte, error) {
	if len(response.Header.Server()) == 0 && !server.NoDefaultServerHeader {
		name := server.Name
		if name == "" {
			name = "fasthttp"
		}
		response.Header.SetServer(name)
	}
	if server.NoDefaultDate {
		serverDate = nil
	}
	return encodeStatusHeaders(
		encoder,
		buffer,
		stringsCache,
		statusCodeString(response.StatusCode()),
		&response.Header,
		serverDate,
		response.Header.PeekTrailerKeys(),
		false,
		maxHeaderListSize,
		scratch,
	)
}

func encodeInformationalHeaders(
	encoder *hpack.Encoder,
	buffer *bytes.Buffer,
	stringsCache *headerStringCache,
	statusCode int,
	header *fasthttp.ResponseHeader,
	maxHeaderListSize uint64,
	scratch *[]hpack.HeaderField,
) ([]byte, error) {
	return encodeStatusHeaders(
		encoder,
		buffer,
		stringsCache,
		statusCodeString(statusCode),
		header,
		nil,
		nil,
		true,
		maxHeaderListSize,
		scratch,
	)
}

// encodeStatusHeaders validates the full field list before anything reaches
// the stateful HPACK encoder, then encodes it. scratch carries the resolved
// fields between the two passes so header.All() runs once.
func encodeStatusHeaders(
	encoder *hpack.Encoder,
	buffer *bytes.Buffer,
	stringsCache *headerStringCache,
	status string,
	header *fasthttp.ResponseHeader,
	serverDate []byte,
	trailerKeys [][]byte,
	skipContentLength bool,
	maxHeaderListSize uint64,
	scratch *[]hpack.HeaderField,
) ([]byte, error) {
	headerSize := uint64(len(":status") + len(status) + 32)
	if len(serverDate) != 0 {
		headerSize += uint64(len(fasthttp.HeaderDate) + len(serverDate) + 32)
	}
	fields := (*scratch)[:0]
	var validateErr error
	header.All()(func(key, value []byte) bool {
		name := stringsCache.name(key)
		if name == "te" {
			validateErr = errors.New("http2: response cannot contain te")
			return false
		}
		if name == "trailer" || isConnectionSpecificHeader(name) {
			return true
		}
		if skipContentLength && name == "content-length" {
			return true
		}
		if len(trailerKeys) != 0 && hasTrailerKey(trailerKeys, name) {
			return true
		}
		if len(serverDate) != 0 && name == "date" {
			return true
		}
		fieldSize := uint64(len(name) + len(value) + 32)
		if maxHeaderListSize != 0 && headerSize+fieldSize > maxHeaderListSize {
			validateErr = errResponseHeaderTooLarge
			return false
		}
		headerSize += fieldSize
		sensitive := isSensitiveHeader(name)
		fields = append(fields, hpack.HeaderField{
			Name:      name,
			Value:     stringsCache.value(value, sensitive),
			Sensitive: sensitive,
		})
		return true
	})
	*scratch = fields
	if validateErr != nil {
		return nil, validateErr
	}

	buffer.Reset()
	if err := encoder.WriteField(hpack.HeaderField{
		Name:  ":status",
		Value: status,
	}); err != nil {
		return nil, err
	}
	if len(serverDate) != 0 {
		if err := encoder.WriteField(hpack.HeaderField{
			Name:  "date",
			Value: stringsCache.value(serverDate, false),
		}); err != nil {
			return nil, err
		}
	}
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			return nil, err
		}
	}
	return buffer.Bytes(), nil
}

// writeContinuationFrames emits block -- the part of a field block its opening
// HEADERS or PUSH_PROMISE frame could not carry -- as CONTINUATIONs of at most
// maxFrameSize bytes (RFC 9113 §6.10).
func writeContinuationFrames(framer *xhttp2.Framer, streamID uint32, block []byte, maxFrameSize int) error {
	for len(block) != 0 {
		length := min(len(block), maxFrameSize)
		if err := framer.WriteContinuation(streamID, length == len(block), block[:length]); err != nil {
			return err
		}
		block = block[length:]
	}
	return nil
}

var statusStrings = func() (table [600]string) {
	for code := range table {
		table[code] = strconv.Itoa(code)
	}
	return table
}()

func statusCodeString(statusCode int) string {
	if statusCode >= 0 && statusCode < len(statusStrings) {
		return statusStrings[statusCode]
	}
	return strconv.Itoa(statusCode)
}

func isConnectionSpecificHeader(name string) bool {
	switch name {
	case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
