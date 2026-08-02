package http2

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2/hpack"
)

var (
	errInvalidRequestHeaders  = errors.New("http2: invalid request headers")
	errRequestBodyTooLarge    = errors.New("http2: request body too large")
	errResponseHeaderTooLarge = errors.New("http2: response header list too large")
)

func populateRequest(
	ctx *fasthttp.RequestCtx,
	server *fasthttp.Server,
	fields []hpack.HeaderField,
	enableExtendedConnect bool,
) (int64, error) {
	if ctx == nil {
		return -1, errInvalidRequestHeaders
	}
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
	seenPseudo := make(map[string]struct{}, 5)
	cookies := make([]string, 0, 1)
	for _, field := range fields {
		if field.IsPseudo() {
			if _, ok := seenPseudo[field.Name]; ok {
				return -1, fmt.Errorf("%w: duplicate pseudo-header %q", errInvalidRequestHeaders, field.Name)
			}
			seenPseudo[field.Name] = struct{}{}
			switch field.Name {
			case ":method":
				method = field.Value
			case ":scheme":
				scheme = field.Value
			case ":authority":
				authority = field.Value
			case ":path":
				path = field.Value
			case ":protocol":
				connectProtocol = field.Value
			default:
				return -1, fmt.Errorf("%w: unknown pseudo-header %q", errInvalidRequestHeaders, field.Name)
			}
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
	if host != "" && host != authority {
		return -1, fmt.Errorf("%w: host and authority differ", errInvalidRequestHeaders)
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

func parseHTTP2ContentLength(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("%w: invalid content-length", errInvalidRequestHeaders)
	}
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
	header *fasthttp.ResponseHeader,
) ([]byte, error) {
	buffer.Reset()
	for _, key := range header.PeekTrailerKeys() {
		name := strings.ToLower(string(key))
		if isConnectionSpecificHeader(name) {
			return nil, fmt.Errorf("http2: invalid response trailer %q", name)
		}
		values := header.PeekAll(string(key))
		for _, value := range values {
			if err := encoder.WriteField(hpack.HeaderField{
				Name:  name,
				Value: string(value),
			}); err != nil {
				return nil, err
			}
		}
	}
	return bytes.Clone(buffer.Bytes()), nil
}

func encodeResponseHeaders(
	encoder *hpack.Encoder,
	buffer *bytes.Buffer,
	server *fasthttp.Server,
	response *fasthttp.Response,
	maxHeaderListSize uint64,
) ([]byte, error) {
	buffer.Reset()
	statusCode := response.StatusCode()
	if statusCode == 0 {
		statusCode = fasthttp.StatusOK
	}
	if err := encoder.WriteField(hpack.HeaderField{
		Name:  ":status",
		Value: strconv.Itoa(statusCode),
	}); err != nil {
		return nil, err
	}
	headerSize := uint64(len(":status") + len(strconv.Itoa(statusCode)) + 32)

	if len(response.Header.Server()) == 0 && !server.NoDefaultServerHeader {
		name := server.Name
		if name == "" {
			name = "fasthttp"
		}
		response.Header.SetServer(name)
	}
	if len(response.Header.Peek(fasthttp.HeaderDate)) == 0 && !server.NoDefaultDate {
		response.Header.SetBytesV(fasthttp.HeaderDate, fasthttp.AppendHTTPDate(nil, time.Now()))
	}

	var encodeErr error
	response.Header.All()(func(key, value []byte) bool {
		name := strings.ToLower(string(key))
		if name == "trailer" || isConnectionSpecificHeader(name) {
			return true
		}
		fieldSize := uint64(len(name) + len(value) + 32)
		if maxHeaderListSize != 0 && headerSize+fieldSize > maxHeaderListSize {
			encodeErr = errResponseHeaderTooLarge
			return false
		}
		headerSize += fieldSize
		encodeErr = encoder.WriteField(hpack.HeaderField{
			Name:      name,
			Value:     string(value),
			Sensitive: name == "set-cookie",
		})
		return encodeErr == nil
	})
	if encodeErr != nil {
		return nil, encodeErr
	}
	return bytes.Clone(buffer.Bytes()), nil
}

func encodeInformationalHeaders(
	encoder *hpack.Encoder,
	buffer *bytes.Buffer,
	statusCode int,
	header *fasthttp.ResponseHeader,
) ([]byte, error) {
	buffer.Reset()
	if err := encoder.WriteField(hpack.HeaderField{
		Name:  ":status",
		Value: strconv.Itoa(statusCode),
	}); err != nil {
		return nil, err
	}
	var encodeErr error
	header.All()(func(key, value []byte) bool {
		name := strings.ToLower(string(key))
		if name == "content-length" || name == "trailer" || isConnectionSpecificHeader(name) {
			return true
		}
		encodeErr = encoder.WriteField(hpack.HeaderField{
			Name:  name,
			Value: string(value),
		})
		return encodeErr == nil
	})
	if encodeErr != nil {
		return nil, encodeErr
	}
	return bytes.Clone(buffer.Bytes()), nil
}

func isConnectionSpecificHeader(name string) bool {
	switch name {
	case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
