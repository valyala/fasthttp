package http2

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2/hpack"
)

var errInvalidResponseHeaders = errors.New("http2: invalid response headers")

func encodeRequestHeaders(
	encoder *hpack.Encoder,
	buffer *bytes.Buffer,
	stringsCache *headerStringCache,
	req *fasthttp.Request,
	maxHeaderListSize uint64,
	enableExtendedConnect bool,
) ([]byte, error) {
	buffer.Reset()
	method := stringsCache.value(req.Header.Method(), false)
	if method == "" {
		method = fasthttp.MethodGet
	}
	uri := req.URI()
	authorityBytes := req.Header.Host()
	if len(authorityBytes) == 0 {
		authorityBytes = uri.Host()
	}
	authority := stringsCache.value(authorityBytes, false)
	if authority == "" {
		return nil, errors.New("http2: request authority is empty")
	}

	headerSize := uint64(0)
	writePseudo := func(name, value string) error {
		headerSize += uint64(len(name) + len(value) + 32)
		if maxHeaderListSize != 0 && headerSize > maxHeaderListSize {
			return errors.New("http2: request header list exceeds peer limit")
		}
		return encoder.WriteField(hpack.HeaderField{Name: name, Value: value})
	}
	if err := writePseudo(":method", method); err != nil {
		return nil, err
	}
	connectProtocol := stringsCache.value(req.Header.ConnectProtocol(), false)
	switch {
	case connectProtocol != "":
		if !enableExtendedConnect || method != fasthttp.MethodConnect {
			return nil, errors.New("http2: invalid extended connect request")
		}
		for _, field := range [...]hpack.HeaderField{
			{Name: ":protocol", Value: connectProtocol},
			{Name: ":scheme", Value: stringsCache.value(uri.Scheme(), false)},
			{Name: ":authority", Value: authority},
			{Name: ":path", Value: stringsCache.value(uri.RequestURI(), false)},
		} {
			if err := writePseudo(field.Name, field.Value); err != nil {
				return nil, err
			}
		}
	case method == fasthttp.MethodConnect:
		if err := writePseudo(":authority", authority); err != nil {
			return nil, err
		}
	default:
		scheme := stringsCache.value(uri.Scheme(), false)
		if scheme == "" {
			scheme = "http"
		}
		path := stringsCache.value(uri.RequestURI(), false)
		if path == "" {
			path = "/"
		}
		for _, field := range [...]hpack.HeaderField{
			{Name: ":scheme", Value: scheme},
			{Name: ":authority", Value: authority},
			{Name: ":path", Value: path},
		} {
			if err := writePseudo(field.Name, field.Value); err != nil {
				return nil, err
			}
		}
	}
	var encodeErr error
	req.Header.All()(func(key, value []byte) bool {
		name := stringsCache.name(key)
		switch name {
		case "host", "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
			return true
		case "te":
			if !strings.EqualFold(strings.TrimSpace(string(value)), "trailers") {
				encodeErr = errors.New("http2: invalid te request header")
				return false
			}
		}
		fieldSize := uint64(len(name) + len(value) + 32)
		if maxHeaderListSize != 0 && headerSize+fieldSize > maxHeaderListSize {
			encodeErr = errors.New("http2: request header list exceeds peer limit")
			return false
		}
		headerSize += fieldSize
		sensitive := isSensitiveHeader(name)
		encodeErr = encoder.WriteField(hpack.HeaderField{
			Name:      name,
			Value:     stringsCache.value(value, sensitive),
			Sensitive: sensitive,
		})
		return encodeErr == nil
	})
	if encodeErr != nil {
		return nil, encodeErr
	}
	return buffer.Bytes(), nil
}

func populateResponse(
	resp *fasthttp.Response,
	fields []hpack.HeaderField,
	disableNormalizing bool,
) (statusCode int, contentLength int64, err error) {
	if disableNormalizing {
		resp.Header.DisableNormalizing()
	}
	status := ""
	contentLength = -1
	seenRegular := false
	for _, field := range fields {
		if field.IsPseudo() {
			if seenRegular || field.Name != ":status" || status != "" {
				return 0, -1, errInvalidResponseHeaders
			}
			status = field.Value
			continue
		}
		seenRegular = true
		name := field.Name
		if isConnectionSpecificHeader(name) {
			return 0, -1, fmt.Errorf("%w: connection-specific header %q", errInvalidResponseHeaders, name)
		}
		if name == "content-length" {
			length, parseErr := parseHTTP2ContentLength(field.Value)
			if parseErr != nil || contentLength >= 0 && contentLength != length {
				return 0, -1, fmt.Errorf("%w: invalid content-length", errInvalidResponseHeaders)
			}
			contentLength = length
		}
		resp.Header.Add(name, field.Value)
	}
	if len(status) != 3 || status[0] < '1' || status[0] > '9' || status[1] < '0' || status[1] > '9' || status[2] < '0' || status[2] > '9' {
		return 0, -1, fmt.Errorf("%w: invalid :status", errInvalidResponseHeaders)
	}
	statusCode, err = strconv.Atoi(status)
	if err != nil {
		return 0, -1, fmt.Errorf("%w: invalid :status", errInvalidResponseHeaders)
	}
	resp.Header.SetStatusCode(statusCode)
	resp.Header.SetProtocol([]byte("HTTP/2"))
	return statusCode, contentLength, nil
}

func responseStatus(fields []hpack.HeaderField) (int, error) {
	status := ""
	for _, field := range fields {
		if field.IsPseudo() {
			if field.Name != ":status" || status != "" {
				return 0, errInvalidResponseHeaders
			}
			status = field.Value
			continue
		}
		if isConnectionSpecificHeader(field.Name) {
			return 0, errInvalidResponseHeaders
		}
	}
	if len(status) != 3 || status[0] < '1' || status[0] > '9' ||
		status[1] < '0' || status[1] > '9' || status[2] < '0' || status[2] > '9' {
		return 0, errInvalidResponseHeaders
	}
	value, err := strconv.Atoi(status)
	if err != nil {
		return 0, errInvalidResponseHeaders
	}
	return value, nil
}

func populateResponseTrailers(resp *fasthttp.Response, fields []hpack.HeaderField) error {
	for _, field := range fields {
		if field.IsPseudo() || isConnectionSpecificHeader(field.Name) {
			return errInvalidResponseHeaders
		}
		if err := resp.Header.AddTrailer(field.Name); err != nil {
			return err
		}
		resp.Header.Set(field.Name, field.Value)
	}
	return nil
}

func populatePromisedRequest(req *fasthttp.Request, fields []hpack.HeaderField) error {
	var method, scheme, authority, path string
	seenPseudo := make(map[string]struct{}, 4)
	for _, field := range fields {
		if field.IsPseudo() {
			if _, ok := seenPseudo[field.Name]; ok {
				return errors.New("http2: duplicate promised request pseudo-header")
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
			default:
				return errors.New("http2: invalid promised request pseudo-header")
			}
			continue
		}
		if isConnectionSpecificHeader(field.Name) || field.Name == "host" {
			return errors.New("http2: invalid promised request header")
		}
		req.Header.Add(field.Name, field.Value)
	}
	if method != fasthttp.MethodGet && method != fasthttp.MethodHead {
		return errors.New("http2: promised request must use GET or HEAD")
	}
	if scheme == "" || authority == "" || path == "" {
		return errors.New("http2: promised request is missing a pseudo-header")
	}
	req.Header.SetMethod(method)
	req.Header.SetHost(authority)
	req.Header.SetProtocol("HTTP/2")
	req.SetRequestURI(scheme + "://" + authority + path)
	_ = req.URI()
	req.Header.SetRequestURI(path)
	return nil
}

func isSensitiveHeader(name string) bool {
	switch name {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	default:
		return false
	}
}
