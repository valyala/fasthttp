package http2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpguts"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	errInvalidRequestHeaders  = errors.New("http2: invalid request headers")
	errRequestBodyTooLarge    = errors.New("http2: request body too large")
	errResponseHeaderTooLarge = errors.New("http2: response header list too large")
	// errHeaderBlockEncode wraps a failure of the HPACK encoder itself. Its
	// dynamic table already changed for a block that never reaches the peer,
	// so the connection cannot go on.
	errHeaderBlockEncode = errors.New("http2: encoding header block")
)

func headerBlockEncodeError(err error) error {
	return fmt.Errorf("%w: %w", errHeaderBlockEncode, err)
}

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
	defaultServer     string
	skipContentLength bool
	// defaultContentType is ResponseHeader.AppendBytes' condition for the
	// default Content-Type; an explicitly set one is always written.
	defaultContentType bool
	maxHeaderListSize  uint64
}

// headerEncoder is the per-connection HPACK encode state: the encoder and its
// output buffer, the string cache, and the field scratch the two passes share.
type headerEncoder struct {
	encoder *hpack.Encoder
	buffer  bytes.Buffer
	strings headerStringCache
	fields  []hpack.HeaderField
	// date interns the Date value, which changes once a second.
	date string
	// scratchHeader holds the copy explicitContentType inspects.
	scratchHeader fasthttp.ResponseHeader
}

func (h *headerEncoder) initHeaderEncoder(maxTableSize uint32) {
	h.encoder = hpack.NewEncoder(&h.buffer)
	h.encoder.SetMaxDynamicTableSizeLimit(maxTableSize)
}

// encodeRequestHeaders encodes req's header block; defaultContentType, when
// set, is the wire-only Content-Type PrepareRequest reported.
func (h *headerEncoder) encodeRequestHeaders(
	req *fasthttp.Request,
	defaultContentType []byte,
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
		if !httpguts.ValidHeaderFieldName(connectProtocol) {
			return nil, errors.New("http2: invalid :protocol token")
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
		if !validWireFieldValue(field.Value) {
			return nil, fmt.Errorf("http2: invalid value for %s", field.Name)
		}
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
		field, err := newWireField(stringsCache, name, value)
		if err != nil {
			validateErr = err
			return false
		}
		fieldSize := uint64(len(name) + len(field.Value) + 32)
		if maxHeaderListSize != 0 && headerSize+fieldSize > maxHeaderListSize {
			validateErr = errors.New("http2: request header list exceeds peer limit")
			return false
		}
		headerSize += fieldSize
		fields = append(fields, field)
		return true
	})
	if validateErr == nil && len(defaultContentType) != 0 {
		field := hpack.HeaderField{Name: "content-type", Value: stringsCache.value(defaultContentType, false)}
		fieldSize := uint64(len(field.Name) + len(field.Value) + 32)
		if maxHeaderListSize != 0 && headerSize+fieldSize > maxHeaderListSize {
			validateErr = errors.New("http2: request header list exceeds peer limit")
		} else {
			headerSize += fieldSize
			fields = append(fields, field)
		}
	}
	*scratch = fields
	if validateErr != nil {
		return nil, validateErr
	}

	buffer.Reset()
	for _, field := range pseudoFields[:pseudoCount] {
		if err := encoder.WriteField(field); err != nil {
			return nil, headerBlockEncodeError(err)
		}
	}
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			return nil, headerBlockEncodeError(err)
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
	// A response left with no Server value gets the default upfront, as in
	// HTTP/1; SetServer would file it as a trailer once Server is announced.
	var defaultServer string
	if len(response.Header.Server()) == 0 {
		defaultServer = defaultServerName(server)
	}
	if server.NoDefaultDate {
		serverDate = nil
	}
	// RFC 9110 §8.6 forbids Content-Length on a 1xx or 204; a 304 keeps
	// one, as ResponseHeader.mustSkipContentLengthOnWire has it.
	statusCode := response.StatusCode()
	return h.encodeStatusHeaders(statusCodeString(statusCode), &response.Header, statusHeaderOptions{
		serverDate:         serverDate,
		defaultServer:      defaultServer,
		skipContentLength:  statusCode < 200 || statusCode == fasthttp.StatusNoContent,
		defaultContentType: writesDefaultContentType(statusCode, response.Header.ContentLength()),
		maxHeaderListSize:  maxHeaderListSize,
	})
}

// writesDefaultContentType is ResponseHeader.AppendBytes' condition for the
// default Content-Type: a body of nonzero or unknown length on a status that
// allows one.
func writesDefaultContentType(statusCode, contentLength int) bool {
	if contentLength == 0 {
		return false
	}
	bodyless := statusCode >= 100 && statusCode < 200 ||
		statusCode == fasthttp.StatusNoContent || statusCode == fasthttp.StatusNotModified
	return !bodyless
}

// explicitContentType reports whether header carries a Content-Type of its
// own rather than the default. The header is copied, not mutated.
func (h *headerEncoder) explicitContentType(header *fasthttp.ResponseHeader) bool {
	header.CopyTo(&h.scratchHeader)
	h.scratchHeader.SetNoDefaultContentType(true)
	return len(h.scratchHeader.ContentType()) != 0
}

// defaultServerName is the Server value HTTP/1 falls back to, newlines
// replaced as SetServer does.
func defaultServerName(server *fasthttp.Server) string {
	name := server.Name
	if name == "" && !server.NoDefaultServerHeader {
		name = "fasthttp"
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, name)
}

// encodeContinueHeaders encodes a bare 100 (Continue) response.
func (h *headerEncoder) encodeContinueHeaders() ([]byte, error) {
	h.buffer.Reset()
	if err := h.encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "100"}); err != nil {
		return nil, headerBlockEncodeError(err)
	}
	return h.buffer.Bytes(), nil
}

func (h *headerEncoder) encodeInformationalHeaders(
	statusCode int,
	header *fasthttp.ResponseHeader,
	maxHeaderListSize uint64,
) ([]byte, error) {
	return h.encodeStatusHeaders(statusCodeString(statusCode), header, statusHeaderOptions{
		skipContentLength:  true,
		defaultContentType: writesDefaultContentType(statusCode, header.ContentLength()),
		maxHeaderListSize:  maxHeaderListSize,
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
	serverDate, defaultServer := opts.serverDate, strings.Trim(opts.defaultServer, " \t")
	skipContentLength, maxHeaderListSize := opts.skipContentLength, opts.maxHeaderListSize
	if !validWireFieldValue(defaultServer) {
		return nil, errors.New("http2: invalid default server header value")
	}
	headerSize := uint64(len(":status") + len(status) + 32)
	if len(serverDate) != 0 {
		headerSize += uint64(len(fasthttp.HeaderDate) + len(serverDate) + 32)
	}
	if defaultServer != "" {
		headerSize += uint64(len(fasthttp.HeaderServer) + len(defaultServer) + 32)
	}
	if maxHeaderListSize != 0 && headerSize > maxHeaderListSize {
		return nil, errResponseHeaderTooLarge
	}
	fields := (*scratch)[:0]
	var validateErr error
	header.All()(func(key, value []byte) bool {
		name := stringsCache.name(key)
		// RFC 9113 §8.2.2: a response generates none of these; TE is dropped
		// with the rest, as HTTP/1 writes it and a request's te: trailers
		// stays valid.
		if name == "te" || isConnectionSpecificHeader(name) {
			return true
		}
		if skipContentLength && name == "content-length" {
			return true
		}
		if len(serverDate) != 0 && name == "date" {
			return true
		}
		if name == "content-type" && !opts.defaultContentType && !h.explicitContentType(header) {
			return true
		}
		field, err := newWireField(stringsCache, name, value)
		if err != nil {
			validateErr = err
			return false
		}
		fieldSize := uint64(len(name) + len(field.Value) + 32)
		if maxHeaderListSize != 0 && headerSize+fieldSize > maxHeaderListSize {
			validateErr = errResponseHeaderTooLarge
			return false
		}
		headerSize += fieldSize
		fields = append(fields, field)
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
		return nil, headerBlockEncodeError(err)
	}
	if len(serverDate) != 0 {
		if h.date != string(serverDate) {
			h.date = string(serverDate)
		}
		if err := encoder.WriteField(hpack.HeaderField{Name: "date", Value: h.date}); err != nil {
			return nil, headerBlockEncodeError(err)
		}
	}
	if defaultServer != "" {
		if err := encoder.WriteField(hpack.HeaderField{Name: "server", Value: defaultServer}); err != nil {
			return nil, headerBlockEncodeError(err)
		}
	}
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			return nil, headerBlockEncodeError(err)
		}
	}
	return buffer.Bytes(), nil
}

// encodeTrailerHeaders encodes a response's trailer section.
func (h *headerEncoder) encodeTrailerHeaders(
	header *fasthttp.ResponseHeader,
	maxHeaderListSize uint64,
) ([]byte, error) {
	return h.encodeTrailerSection(header.TrailerHeader(), "response", maxHeaderListSize, errResponseHeaderTooLarge)
}

// encodeTrailerSection encodes a serialized trailer section, as HTTP/1 writes
// it: a header-section value of an announced name went out in the header
// block. kind names the message in errors; limitErr reports the peer's list
// limit.
func (h *headerEncoder) encodeTrailerSection(
	section []byte,
	kind string,
	maxHeaderListSize uint64,
	limitErr error,
) ([]byte, error) {
	encoder, buffer, stringsCache, scratch := h.encoder, &h.buffer, &h.strings, &h.fields
	headerSize := uint64(0)
	fields := (*scratch)[:0]
	for key, value, rest := nextHeaderLine(section); key != nil; key, value, rest = nextHeaderLine(rest) {
		name := stringsCache.name(key)
		if name == "te" || isConnectionSpecificHeader(name) {
			*scratch = fields
			return nil, fmt.Errorf("http2: invalid %s trailer %q", kind, name)
		}
		field, err := newWireField(stringsCache, name, value)
		if err != nil {
			*scratch = fields
			return nil, err
		}
		headerSize += uint64(len(name) + len(field.Value) + 32)
		if maxHeaderListSize != 0 && headerSize > maxHeaderListSize {
			*scratch = fields
			return nil, limitErr
		}
		fields = append(fields, field)
	}
	*scratch = fields
	buffer.Reset()
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			return nil, headerBlockEncodeError(err)
		}
	}
	return buffer.Bytes(), nil
}

// newWireField prepares a regular field for the wire. Surrounding SP and HTAB,
// which RFC 9113 §8.2.1 forbids and an HTTP/1 recipient drops, are trimmed;
// anything else invalid fails the message.
func newWireField(stringsCache *headerStringCache, name string, value []byte) (hpack.HeaderField, error) {
	// name was lowercased, which leaves only the token check.
	if !httpguts.ValidHeaderFieldName(name) {
		return hpack.HeaderField{}, fmt.Errorf("http2: invalid header field name %q", name)
	}
	for len(value) != 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	for len(value) != 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	if !validFieldValueBytes(value) {
		return hpack.HeaderField{}, fmt.Errorf("http2: invalid value for header field %q", name)
	}
	sensitive := isSensitiveHeader(name)
	return hpack.HeaderField{
		Name:      name,
		Value:     stringsCache.value(value, sensitive),
		Sensitive: sensitive,
	}, nil
}

// validFieldValueBytes is httpguts.ValidHeaderFieldValue a word at a time.
func validFieldValueBytes(value []byte) bool {
	const lowBits, highBits = 0x0101010101010101, 0x8080808080808080
	for len(value) >= 8 {
		w := binary.LittleEndian.Uint64(value)
		del := w ^ 0x7f*lowBits
		if ((w-0x20*lowBits)&^w|(del-lowBits)&^del)&highBits != 0 {
			break
		}
		value = value[8:]
	}
	for _, b := range value {
		if b < ' ' && b != '\t' || b == 0x7f {
			return false
		}
	}
	return true
}

// validWireFieldValue is the receive path's rule for a field value.
func validWireFieldValue(value string) bool {
	return httpguts.ValidHeaderFieldValue(value) && !hasEdgeWhitespace(value)
}

// nextHeaderLine splits the first "Key: value\r\n" line off a serialized
// section; key is nil at the closing empty line. Keys are tokens and values
// hold no line breaks, so the split is exact.
func nextHeaderLine(b []byte) (key, value, rest []byte) {
	end := bytes.IndexByte(b, '\r')
	if end <= 0 {
		return nil, nil, nil
	}
	line := b[:end]
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 || colon+len(": ") > len(line) {
		return nil, nil, nil
	}
	return line[:colon], line[colon+len(": "):], b[end+len("\r\n"):]
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

// applyRequestTrailers records a trailer block in header. An undeclared name
// is declared through the parser's trailer rules first, so Add files its
// value in the trailer section.
func applyRequestTrailers(
	protocolContext *fasthttp.ProtocolServerContext,
	header *fasthttp.RequestHeader,
	fields []hpack.HeaderField,
) error {
	// The scan path re-reads the registered list per field because the
	// declaration appends to it; the index path must not pay for that read.
	known := newTrailerIndex(header.PeekTrailerKeys(), len(fields))
	for _, field := range fields {
		var isNew bool
		if known != nil {
			isNew = indexTrailerKey(known, field.Name)
		} else {
			isNew = !hasTrailerKey(header.PeekTrailerKeys(), field.Name)
		}
		if isNew {
			if err := protocolContext.ParseTrailer(header, s2b(field.Name)); err != nil {
				return err
			}
		}
		header.Add(field.Name, field.Value)
	}
	return nil
}

// s2b is the read-only byte view of s.
func s2b(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// parsedRequest is what populateRequest reads off a header block besides the
// header fields themselves.
type parsedRequest struct {
	// contentLength is the declared length, or -1.
	contentLength int64
	// priority is the Priority field value, its lines combined.
	priority string
	// refusal is ErrBadTrailer for a Trailer declaration naming a forbidden
	// field: the request is well-formed and answered through ErrorHandler,
	// as HTTP/1 answers it.
	refusal error
}

// populateRequest fills ctx.Request from a decoded header block. A malformed
// block is the returned error; Trailer declarations are recorded after the
// ordinary fields, with the HTTP/1 parser's rules.
func populateRequest(
	ctx *fasthttp.RequestCtx,
	fields []hpack.HeaderField,
	protocolContext *fasthttp.ProtocolServerContext,
	enableExtendedConnect bool,
) (parsedRequest, error) {
	var method string
	var scheme string
	var authority string
	var path string
	var connectProtocol string
	var host string
	var priority string
	var hasPriority bool
	var contentLength int64 = -1
	var seenPseudo pseudoField
	var trailers []string
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
				return parsedRequest{}, fmt.Errorf("%w: unknown pseudo-header %q", errInvalidRequestHeaders, field.Name)
			}
			if seenPseudo&bit != 0 {
				return parsedRequest{}, fmt.Errorf("%w: duplicate pseudo-header %q", errInvalidRequestHeaders, field.Name)
			}
			seenPseudo |= bit
			continue
		}

		name := field.Name
		value := field.Value
		if isConnectionSpecificHeader(name) {
			return parsedRequest{}, fmt.Errorf("%w: connection-specific header %q", errInvalidRequestHeaders, name)
		}
		switch name {
		case "te":
			if !strings.EqualFold(strings.TrimSpace(value), "trailers") {
				return parsedRequest{}, fmt.Errorf("%w: invalid te header", errInvalidRequestHeaders)
			}
		case "content-length":
			parsed, err := parseHTTP2ContentLength(value)
			if err != nil {
				return parsedRequest{}, err
			}
			// A second field is malformed, identical or not, as HTTP/1's
			// parser has it.
			if contentLength >= 0 {
				return parsedRequest{}, fmt.Errorf("%w: duplicate content-length", errInvalidRequestHeaders)
			}
			contentLength = parsed
		case "host":
			host = value
			continue
		case "cookie":
			cookies = append(cookies, value)
			continue
		case "trailer":
			trailers = append(trailers, value)
			continue
		// Add would route these to the same setters after normalizing and
		// copying the field twice.
		case "user-agent":
			ctx.Request.Header.SetUserAgent(value)
			continue
		case "content-type":
			ctx.Request.Header.SetContentType(value)
			continue
		case "priority":
			// Repeated field lines form one list (RFC 8941 §4.2); an empty
			// line keeps its place in it.
			if hasPriority {
				priority += "," + value
			} else {
				priority, hasPriority = value, true
			}
		}
		ctx.Request.Header.Add(name, value)
	}

	if method == "" {
		return parsedRequest{}, fmt.Errorf("%w: missing :method", errInvalidRequestHeaders)
	}
	if !httpguts.ValidHeaderFieldName(method) {
		return parsedRequest{}, fmt.Errorf("%w: invalid :method", errInvalidRequestHeaders)
	}
	isConnect := method == fasthttp.MethodConnect
	// Presence, not emptiness: an empty field that must be omitted still
	// makes the request malformed.
	switch {
	case seenPseudo&pseudoProtocol != 0:
		if !enableExtendedConnect {
			return parsedRequest{}, fmt.Errorf("%w: extended connect is disabled", errInvalidRequestHeaders)
		}
		if !isConnect || scheme == "" || authority == "" || path == "" {
			return parsedRequest{}, fmt.Errorf("%w: malformed extended connect", errInvalidRequestHeaders)
		}
		// RFC 8441 §4: one protocol-name token, which an empty value isn't.
		if !httpguts.ValidHeaderFieldName(connectProtocol) {
			return parsedRequest{}, fmt.Errorf("%w: invalid :protocol", errInvalidRequestHeaders)
		}
	case isConnect:
		if authority == "" || seenPseudo&(pseudoScheme|pseudoPath) != 0 {
			return parsedRequest{}, fmt.Errorf("%w: malformed connect", errInvalidRequestHeaders)
		}
	default:
		if scheme == "" || path == "" {
			return parsedRequest{}, fmt.Errorf("%w: missing request pseudo-header", errInvalidRequestHeaders)
		}
	}
	if authority == "" {
		authority = host
	}
	if authority == "" {
		return parsedRequest{}, fmt.Errorf("%w: missing authority", errInvalidRequestHeaders)
	}
	if !httpguts.ValidHostHeader(authority) {
		return parsedRequest{}, fmt.Errorf("%w: invalid authority", errInvalidRequestHeaders)
	}
	if host != "" && host != authority {
		if !strings.EqualFold(host, authority) {
			return parsedRequest{}, fmt.Errorf("%w: host and authority differ", errInvalidRequestHeaders)
		}
	}
	if err := validateRequestTarget(method, scheme, path); err != nil {
		return parsedRequest{}, err
	}

	ctx.Request.Header.SetMethod(method)
	ctx.Request.Header.SetProtocol("HTTP/2")
	ctx.Request.Header.SetHost(authority)
	switch {
	case path != "":
		ctx.Request.Header.SetRequestURI(path)
	case isConnect:
		// The HTTP/1 request target of a CONNECT is the authority.
		ctx.Request.Header.SetRequestURI(authority)
	}
	if connectProtocol != "" {
		ctx.Request.Header.SetConnectProtocol(connectProtocol)
	}
	if len(cookies) != 0 {
		ctx.Request.Header.Set(fasthttp.HeaderCookie, strings.Join(cookies, "; "))
	}
	if contentLength < 0 {
		// HTTP/1's state for a request without a declared length: -2 and
		// no framing field. The setter files Transfer-Encoding for any
		// negative value; body framing moves this to -1 later.
		ctx.Request.Header.SetContentLength(-2)
		ctx.Request.Header.Del(fasthttp.HeaderTransferEncoding)
	}
	// The lazy URI parse yields the transport's scheme; :scheme overrides it
	// only when it differs, as HTTP/1's absolute-form does.
	if scheme != "" && scheme != transportScheme(ctx.IsTLS()) {
		ctx.Request.URI().SetScheme(scheme)
	}
	result := parsedRequest{contentLength: contentLength, priority: priority}
	// Declared last: no header-section field is routed by a declaration.
	// The first error stands; valid names in later lines are still recorded.
	for _, value := range trailers {
		if err := protocolContext.ParseTrailer(&ctx.Request.Header, s2b(value)); err != nil && result.refusal == nil {
			result.refusal = err
		}
	}
	return result, nil
}

// populateTruncatedRequest fills ctx.Request with the pseudo-headers a block
// cut at MaxHeaderListSize decoded, for Server.ErrorHandler; an invalid value
// is left out rather than reported.
func populateTruncatedRequest(ctx *fasthttp.RequestCtx, fields []hpack.HeaderField) {
	header := &ctx.Request.Header
	header.SetProtocol("HTTP/2")
	for _, field := range fields {
		if !field.IsPseudo() {
			return
		}
		switch field.Name {
		case ":method":
			if httpguts.ValidHeaderFieldName(field.Value) {
				header.SetMethod(field.Value)
			}
		case ":authority":
			if httpguts.ValidHostHeader(field.Value) {
				header.SetHost(field.Value)
			}
		case ":path":
			if field.Value != "" && field.Value[0] == '/' {
				header.SetRequestURI(field.Value)
			}
		}
	}
}

func transportScheme(isTLS bool) string {
	if isTLS {
		return "https"
	}
	return "http"
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
	if err != nil || uint64(length) > uint64(^uint(0)>>1) { // #nosec G115
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

// validatePromisedField applies RFC 9113 §8.4 to a promised request field:
// no connection-specific fields, no content. name is lowercase.
func validatePromisedField(name, value string) error {
	switch {
	case isConnectionSpecificHeader(name) || name == "host":
		return errors.New("http2: invalid promised request header")
	case name == "te":
		if !strings.EqualFold(strings.TrimSpace(value), "trailers") {
			return errors.New("http2: invalid te header in promised request")
		}
	case name == "content-length":
		if length, err := parseHTTP2ContentLength(value); err != nil || length != 0 {
			return errors.New("http2: promised request indicates content")
		}
	}
	return nil
}

// validateRequestTarget checks :path and :scheme of a request that has them
// (RFC 9113 §8.3.1); CONNECT's omission rules are the caller's.
func validateRequestTarget(method, scheme, path string) error {
	if path != "" && path != "*" && path[0] != '/' {
		return fmt.Errorf("%w: invalid :path", errInvalidRequestHeaders)
	}
	if path == "*" && method != fasthttp.MethodOptions {
		return fmt.Errorf("%w: asterisk :path requires OPTIONS", errInvalidRequestHeaders)
	}
	if scheme != "" && !validScheme(scheme) {
		return fmt.Errorf("%w: invalid :scheme", errInvalidRequestHeaders)
	}
	return nil
}

func isConnectionSpecificHeader(name string) bool {
	switch name {
	case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
