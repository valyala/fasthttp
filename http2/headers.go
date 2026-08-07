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

// pseudoField is a bit per request pseudo-header, for duplicate detection.
type pseudoField uint8

const (
	pseudoMethod pseudoField = 1 << iota
	pseudoScheme
	pseudoAuthority
	pseudoPath
	pseudoProtocol
)

// trailerIndexThreshold bounds the scan-based trailer dedup: names are
// peer-controlled and thousands fit under the header-list limit.
const trailerIndexThreshold = 16

// statusHeaderOptions carries what varies between a final response and an
// informational one.
type statusHeaderOptions struct {
	serverDate        []byte
	trailerKeys       [][]byte
	skipContentLength bool
	maxHeaderListSize uint64
}

// headerEncoder is the per-connection HPACK encode state: the encoder and its
// output buffer, the string cache, and the field scratch the two passes share.
type headerEncoder struct {
	encoder *hpack.Encoder
	buffer  bytes.Buffer
	strings headerStringCache
	fields  []hpack.HeaderField
}

func (h *headerEncoder) initHeaderEncoder(maxTableSize uint32) {
	h.encoder = hpack.NewEncoder(&h.buffer)
	h.encoder.SetMaxDynamicTableSizeLimit(maxTableSize)
}

func (h *headerEncoder) encodeRequestHeaders(
	req *fasthttp.Request,
	maxHeaderListSize uint64,
	enableExtendedConnect bool,
) ([]byte, error) {
	encoder, buffer, stringsCache, scratch := h.encoder, &h.buffer, &h.strings, &h.fields
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
	// The HPACK encoder is stateful: nothing reaches it until the whole block fits.
	fields := (*scratch)[:0]
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
	for _, field := range pseudoFields[:pseudoCount] {
		if err := encoder.WriteField(field); err != nil {
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

func (h *headerEncoder) encodeResponseHeaders(
	server *fasthttp.Server,
	response *fasthttp.Response,
	maxHeaderListSize uint64,
	serverDate []byte,
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
	return h.encodeStatusHeaders(statusCodeString(response.StatusCode()), &response.Header, statusHeaderOptions{
		serverDate:        serverDate,
		trailerKeys:       response.Header.PeekTrailerKeys(),
		maxHeaderListSize: maxHeaderListSize,
	})
}

func (h *headerEncoder) encodeInformationalHeaders(
	statusCode int,
	header *fasthttp.ResponseHeader,
	maxHeaderListSize uint64,
) ([]byte, error) {
	return h.encodeStatusHeaders(statusCodeString(statusCode), header, statusHeaderOptions{
		skipContentLength: true,
		maxHeaderListSize: maxHeaderListSize,
	})
}

// encodeStatusHeaders validates the whole field list before anything reaches
// the stateful HPACK encoder.
func (h *headerEncoder) encodeStatusHeaders(
	status string,
	header *fasthttp.ResponseHeader,
	opts statusHeaderOptions,
) ([]byte, error) {
	encoder, buffer, stringsCache, scratch := h.encoder, &h.buffer, &h.strings, &h.fields
	serverDate, trailerKeys := opts.serverDate, opts.trailerKeys
	skipContentLength, maxHeaderListSize := opts.skipContentLength, opts.maxHeaderListSize
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

func (h *headerEncoder) encodeTrailerHeaders(
	header *fasthttp.ResponseHeader,
	maxHeaderListSize uint64,
) ([]byte, error) {
	encoder, buffer, stringsCache := h.encoder, &h.buffer, &h.strings
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

// newTrailerIndex returns a name index once the block is large enough that a
// scan per field would turn quadratic, and nil while scanning stays cheaper.
func newTrailerIndex(keys [][]byte, incoming int) map[string]struct{} {
	if len(keys)+incoming <= trailerIndexThreshold {
		return nil
	}
	known := make(map[string]struct{}, len(keys)+incoming)
	for _, key := range keys {
		known[strings.ToLower(string(key))] = struct{}{}
	}
	return known
}

// indexTrailerKey reports whether name is new to known and records it. HTTP/2
// field names are lowercase, so the index needs no folding.
func indexTrailerKey(known map[string]struct{}, name string) bool {
	if _, exists := known[name]; exists {
		return false
	}
	known[name] = struct{}{}
	return true
}

func applyRequestTrailers(header *fasthttp.RequestHeader, fields []hpack.HeaderField) error {
	// The scan path re-reads the registered list per field because AddTrailer
	// appends to it; the index path must not pay for that read.
	known := newTrailerIndex(header.PeekTrailerKeys(), len(fields))
	for _, field := range fields {
		var isNew bool
		if known != nil {
			isNew = indexTrailerKey(known, field.Name)
		} else {
			isNew = !hasTrailerKey(header.PeekTrailerKeys(), field.Name)
		}
		if isNew {
			if err := header.AddTrailer(field.Name); err != nil {
				return err
			}
		}
		header.Add(field.Name, field.Value)
	}
	return nil
}

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
	var seenPseudo pseudoField
	cookies := make([]string, 0, 1)
	for _, field := range fields {
		if field.IsPseudo() {
			var bit pseudoField
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
