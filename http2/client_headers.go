package http2

import (
	"bytes"
	"errors"
	"fmt"
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
	method := stringsCache.value(req.Header.Method(), false)
	uri := req.URI()
	authorityBytes := req.Header.Host()
	if len(authorityBytes) == 0 {
		authorityBytes = uri.Host()
	}
	authority := stringsCache.value(authorityBytes, false)
	if authority == "" {
		return nil, errors.New("http2: request authority is empty")
	}

	var pseudoFields [5]hpack.HeaderField
	pseudoCount := 1
	pseudoFields[0] = hpack.HeaderField{Name: ":method", Value: method}
	addPseudo := func(name, value string) {
		pseudoFields[pseudoCount] = hpack.HeaderField{Name: name, Value: value}
		pseudoCount++
	}

	connectProtocol := stringsCache.value(req.Header.ConnectProtocol(), false)
	switch {
	case connectProtocol != "":
		if !enableExtendedConnect || method != fasthttp.MethodConnect {
			return nil, errors.New("http2: invalid extended connect request")
		}
		addPseudo(":protocol", connectProtocol)
		addPseudo(":scheme", stringsCache.value(uri.Scheme(), false))
		addPseudo(":authority", authority)
		addPseudo(":path", stringsCache.value(uri.RequestURI(), false))
	case method == fasthttp.MethodConnect:
		addPseudo(":authority", authority)
	default:
		scheme := stringsCache.value(uri.Scheme(), false)
		path := stringsCache.value(uri.RequestURI(), false)
		if path == "" {
			path = "/"
		}
		addPseudo(":scheme", scheme)
		addPseudo(":authority", authority)
		addPseudo(":path", path)
	}

	headerSize := uint64(0)
	for _, field := range pseudoFields[:pseudoCount] {
		headerSize += uint64(len(field.Name) + len(field.Value) + 32)
		if maxHeaderListSize != 0 && headerSize > maxHeaderListSize {
			return nil, errors.New("http2: request header list exceeds peer limit")
		}
	}
	var validateErr error
	req.Header.All()(func(key, value []byte) bool {
		name := stringsCache.name(key)
		switch name {
		case "host", "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
			return true
		case "te":
			if !strings.EqualFold(strings.TrimSpace(string(value)), "trailers") {
				validateErr = errors.New("http2: invalid te request header")
				return false
			}
		}
		fieldSize := uint64(len(name) + len(value) + 32)
		if maxHeaderListSize != 0 && headerSize+fieldSize > maxHeaderListSize {
			validateErr = errors.New("http2: request header list exceeds peer limit")
			return false
		}
		headerSize += fieldSize
		return true
	})
	if validateErr != nil {
		return nil, validateErr
	}

	buffer.Reset()
	for _, field := range pseudoFields[:pseudoCount] {
		if err := encoder.WriteField(field); err != nil {
			return nil, err
		}
	}
	var encodeErr error
	req.Header.All()(func(key, value []byte) bool {
		name := stringsCache.name(key)
		switch name {
		case "host", "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
			return true
		}
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
		if isConnectionSpecificHeader(name) || name == "te" {
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
	if len(status) != 3 || status[0] < '1' || status[0] > '9' ||
		status[1] < '0' || status[1] > '9' || status[2] < '0' || status[2] > '9' {
		return 0, -1, fmt.Errorf("%w: invalid :status", errInvalidResponseHeaders)
	}
	statusCode = statusCodeFromDigits(status)
	resp.Header.SetStatusCode(statusCode)
	resp.Header.SetProtocol([]byte("HTTP/2"))
	return statusCode, contentLength, nil
}

// responseStatus validates :status. Pseudo-headers come first, so the scan
// stops at the first regular field; only 1xx needs the rest checked, since it
// returns before populateResponse runs.
func responseStatus(fields []hpack.HeaderField) (int, error) {
	status := ""
	regular := 0
	for ; regular < len(fields) && fields[regular].IsPseudo(); regular++ {
		if fields[regular].Name != ":status" || status != "" {
			return 0, errInvalidResponseHeaders
		}
		status = fields[regular].Value
	}
	if len(status) != 3 || status[0] < '1' || status[0] > '9' ||
		status[1] < '0' || status[1] > '9' || status[2] < '0' || status[2] > '9' {
		return 0, errInvalidResponseHeaders
	}
	value := statusCodeFromDigits(status)
	if value >= 100 && value < 200 {
		for _, field := range fields[regular:] {
			if isConnectionSpecificHeader(field.Name) || field.Name == "te" ||
				field.Name == "content-length" {
				return 0, errInvalidResponseHeaders
			}
		}
	}
	return value, nil
}

// statusCodeFromDigits assumes callers validated the three digits.
func statusCodeFromDigits(status string) int {
	return int(status[0]-'0')*100 + int(status[1]-'0')*10 + int(status[2]-'0')
}

func populateResponseTrailers(resp *fasthttp.Response, fields []hpack.HeaderField) error {
	for _, field := range fields {
		if field.IsPseudo() || isConnectionSpecificHeader(field.Name) || field.Name == "te" {
			return errInvalidResponseHeaders
		}
		if !hasTrailerKey(resp.Header.PeekTrailerKeys(), field.Name) {
			if err := resp.Header.AddTrailer(field.Name); err != nil {
				return err
			}
		}
		resp.Header.Add(field.Name, field.Value)
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

func hasTrailerKey(keys [][]byte, name string) bool {
	for _, key := range keys {
		if len(key) == len(name) && bytes.EqualFold(key, []byte(name)) {
			return true
		}
	}
	return false
}

func isSensitiveHeader(name string) bool {
	switch name {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	default:
		return false
	}
}
