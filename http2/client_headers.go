package http2

import (
	"errors"
	"fmt"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http2/hpack"
)

var errInvalidResponseHeaders = errors.New("http2: invalid response headers")

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
			if err := resp.Header.AddTrailer(field.Name); err != nil {
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

func populatePromisedRequest(req *fasthttp.Request, fields []hpack.HeaderField) error {
	var method, scheme, authority, path string
	var seenPseudo pseudoField
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
