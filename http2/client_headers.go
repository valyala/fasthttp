package http2

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2/hpack"
)

var errInvalidResponseHeaders = errors.New("http2: invalid response headers")

// populateResponse fills resp from a final response header block; lease
// records the Trailer declarations.
func populateResponse(
	lease *fasthttp.ProtocolClientConn,
	resp *fasthttp.Response,
	fields []hpack.HeaderField,
	disableNormalizing bool,
) (statusCode int, contentLength int64, err error) {
	if disableNormalizing {
		resp.Header.DisableNormalizing()
	}
	// A neutral status first: the length setter is a no-op on the 204/304 a
	// reused Response may still carry.
	resp.Header.SetStatusCode(fasthttp.StatusOK)
	status := ""
	contentLength = -1
	seenRegular := false
	var trailers []string
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
			// A second field is malformed, as HTTP/1's parser has it.
			length, parseErr := parseHTTP2ContentLength(field.Value)
			if parseErr != nil || contentLength >= 0 {
				return 0, -1, fmt.Errorf("%w: invalid content-length", errInvalidResponseHeaders)
			}
			contentLength = length
		}
		if name == "trailer" {
			// Applied after the ordinary fields, so none is routed by it.
			trailers = append(trailers, field.Value)
			continue
		}
		resp.Header.Add(name, field.Value)
	}
	if len(status) != 3 || status[0] < '1' || status[0] > '9' ||
		status[1] < '0' || status[1] > '9' || status[2] < '0' || status[2] > '9' {
		return 0, -1, fmt.Errorf("%w: invalid :status", errInvalidResponseHeaders)
	}
	for _, trailer := range trailers {
		if parseErr := lease.ParseTrailer(&resp.Header, []byte(trailer)); parseErr != nil && err == nil {
			err = fmt.Errorf("%w: %w", errInvalidResponseHeaders, parseErr)
		}
	}
	if err != nil {
		return 0, -1, err
	}
	// Without a content-length the response reads as HTTP/1 reads a chunked
	// one: -1 with a Transfer-Encoding entry; the setter also removes that
	// entry next to a declared length, which Header.Add doesn't.
	resp.Header.SetContentLength(int(contentLength))
	statusCode = statusCodeFromDigits(status)
	resp.Header.SetStatusCode(statusCode)
	// HTTP/2 carries no reason phrase; the HTTP/1 client reads the one its
	// server emitted, so the same text is supplied here.
	resp.Header.SetStatusMessage(statusMessageBytes(statusCode))
	resp.Header.SetProtocol([]byte("HTTP/2"))
	return statusCode, contentLength, nil
}

// statusMessages holds fasthttp.StatusMessage's text as bytes; index 0 is the
// unknown-code fallback.
var statusMessages = func() (table [600][]byte) {
	for code := range table {
		table[code] = []byte(fasthttp.StatusMessage(code))
	}
	return table
}()

func statusMessageBytes(statusCode int) []byte {
	if statusCode > 0 && statusCode < len(statusMessages) {
		return statusMessages[statusCode]
	}
	return statusMessages[0]
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

func populateResponseTrailers(lease *fasthttp.ProtocolClientConn, resp *fasthttp.Response, fields []hpack.HeaderField) error {
	if err := validateResponseTrailerFields(fields); err != nil {
		return err
	}
	known := newTrailerIndex(resp.Header.PeekTrailerKeys(), len(fields))
	for _, field := range fields {
		var isNew bool
		if known != nil {
			isNew = indexTrailerKey(known, field.Name)
		} else {
			isNew = !hasTrailerKey(resp.Header.PeekTrailerKeys(), field.Name)
		}
		if isNew {
			if err := lease.ParseTrailer(&resp.Header, []byte(field.Name)); err != nil {
				return err
			}
		}
		resp.Header.Add(field.Name, field.Value)
	}
	return nil
}

func validateResponseTrailerFields(fields []hpack.HeaderField) error {
	for _, field := range fields {
		if field.IsPseudo() || isConnectionSpecificHeader(field.Name) || field.Name == "te" {
			return errInvalidResponseHeaders
		}
	}
	return nil
}

// sameOrigin reports whether promised has the scheme and authority parent was
// sent with.
func sameOrigin(parent, promised *fasthttp.Request) bool {
	authority := parent.Header.Host()
	if len(authority) == 0 {
		authority = parent.URI().Host()
	}
	return bytes.EqualFold(parent.URI().Scheme(), promised.URI().Scheme()) &&
		bytes.EqualFold(authority, promised.Header.Host())
}

func populatePromisedRequest(req *fasthttp.Request, fields []hpack.HeaderField) error {
	var method, scheme, authority, path string
	var seenPseudo pseudoField
	declaredLength := false
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
			default:
				return errors.New("http2: invalid promised request pseudo-header")
			}
			if seenPseudo&bit != 0 {
				return errors.New("http2: duplicate promised request pseudo-header")
			}
			seenPseudo |= bit
			continue
		}
		if err := validatePromisedField(field.Name, field.Value); err != nil {
			return err
		}
		if field.Name == "content-length" {
			if declaredLength {
				return errors.New("http2: duplicate content-length in promised request")
			}
			declaredLength = true
		}
		req.Header.Add(field.Name, field.Value)
	}
	if !declaredLength {
		// HTTP/1's "no declared length" state; the setter files a
		// Transfer-Encoding entry a bodyless request doesn't carry.
		req.Header.SetContentLength(-2)
		req.Header.Del(fasthttp.HeaderTransferEncoding)
	}
	if method != fasthttp.MethodGet && method != fasthttp.MethodHead {
		return errors.New("http2: promised request must use GET or HEAD")
	}
	if scheme == "" || authority == "" || path == "" {
		return errors.New("http2: promised request is missing a pseudo-header")
	}
	if err := validateRequestTarget(method, scheme, path); err != nil {
		return err
	}
	req.Header.SetMethod(method)
	req.Header.SetHost(authority)
	req.Header.SetProtocol("HTTP/2")
	req.SetRequestURI(scheme + "://" + authority + path)
	_ = req.URI()
	req.Header.SetRequestURI(path)
	return nil
}

var errRequestTrailerTooLarge = errors.New("http2: request trailer list exceeds peer limit")

// encodeRequestTrailers encodes the request's trailer section, as HTTP/1
// writes it after a chunked body.
func (h *headerEncoder) encodeRequestTrailers(
	header *fasthttp.RequestHeader,
	maxHeaderListSize uint64,
) ([]byte, error) {
	return h.encodeTrailerSection(header.TrailerHeader(), "request", maxHeaderListSize, errRequestTrailerTooLarge)
}
