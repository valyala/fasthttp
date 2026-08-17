package http2

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/http/httpguts"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	maxCachedHeaderNames  = 128
	maxCachedHeaderValues = 256
	maxCachedHeaderValue  = 4 << 10
	maxContinuationFrames = 64
)

type headerStringCache struct {
	names  map[string]string
	values map[string]string
}

func (c *headerStringCache) name(value []byte) string {
	if cached, ok := c.names[string(value)]; ok {
		return cached
	}
	original := string(value)
	lower := strings.ToLower(original)
	if c.names == nil {
		c.names = make(map[string]string, 16)
	} else if len(c.names) >= maxCachedHeaderNames {
		clear(c.names)
	}
	c.names[original] = lower
	return lower
}

func (c *headerStringCache) value(value []byte, sensitive bool) string {
	if sensitive || len(value) > maxCachedHeaderValue {
		return string(value)
	}
	if cached, ok := c.values[string(value)]; ok {
		return cached
	}
	stable := string(value)
	if c.values == nil {
		c.values = make(map[string]string, 32)
	} else if len(c.values) >= maxCachedHeaderValues {
		clear(c.values)
	}
	c.values[stable] = stable
	return stable
}

type headerBlockFrame interface {
	HeaderBlockFragment() []byte
	HeadersEnded() bool
}

// headerCodec owns the connection-scoped HPACK decoder. It decodes directly
// into caller-provided pooled storage instead of asking x/net Framer to build a
// second MetaHeadersFrame field slice for every message.
type headerCodec struct {
	decoder *hpack.Decoder

	fields      []hpack.HeaderField
	remaining   uint64
	truncated   bool
	invalid     error
	sawRegular  bool
	maxListSize uint64
}

func newHeaderCodec(maxTableSize, maxListSize uint32) *headerCodec {
	codec := &headerCodec{maxListSize: uint64(maxListSize)}
	codec.decoder = hpack.NewDecoder(maxTableSize, codec.emit)
	codec.decoder.SetAllowedMaxDynamicTableSize(maxTableSize)
	codec.decoder.SetMaxStringLength(int(codec.maxListSize))
	return codec
}

// decode consumes one header block, CONTINUATIONs included, even when invalid,
// to keep the HPACK table in sync (RFC 9113 §4.3). invalid poisons one stream;
// err is fatal to the connection.
func (c *headerCodec) decode(
	framer *xhttp2.Framer,
	streamID uint32,
	first headerBlockFrame,
	fields []hpack.HeaderField,
) ([]hpack.HeaderField, bool, error, error) {
	c.fields = fields
	c.remaining = c.maxListSize
	c.truncated = false
	c.invalid = nil
	c.sawRegular = false
	c.decoder.SetEmitEnabled(true)
	compressedLimit := 2 * c.maxListSize
	compressedBytes := uint64(0)
	continuations := 0

	current := first
	for {
		fragment := current.HeaderBlockFragment()
		fragmentSize := uint64(len(fragment))
		if fragmentSize > compressedLimit-compressedBytes {
			decodedFields := c.fields
			c.resetBlock()
			return decodedFields, false, nil, xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
		}
		compressedBytes += fragmentSize
		if _, err := c.decoder.Write(fragment); err != nil {
			decodedFields := c.fields
			c.resetBlock()
			return decodedFields, false, nil, xhttp2.ConnectionError(xhttp2.ErrCodeCompression)
		}
		if current.HeadersEnded() {
			break
		}
		if continuations >= maxContinuationFrames {
			decodedFields := c.fields
			c.resetBlock()
			return decodedFields, false, nil, xhttp2.ConnectionError(xhttp2.ErrCodeEnhanceYourCalm)
		}
		continuations++
		frame, err := framer.ReadFrame()
		if err != nil {
			decodedFields := c.fields
			c.resetBlock()
			return decodedFields, false, nil, err
		}
		continuation, ok := frame.(*xhttp2.ContinuationFrame)
		if !ok || continuation.StreamID != streamID {
			decodedFields := c.fields
			c.resetBlock()
			return decodedFields, false, nil, xhttp2.ConnectionError(xhttp2.ErrCodeProtocol)
		}
		current = continuation
	}
	if err := c.decoder.Close(); err != nil {
		decodedFields := c.fields
		c.resetBlock()
		return decodedFields, false, nil, xhttp2.ConnectionError(xhttp2.ErrCodeCompression)
	}
	decodedFields := c.fields
	truncated := c.truncated
	invalid := c.invalid
	c.resetBlock()
	return decodedFields, truncated, invalid, nil
}

func (c *headerCodec) emit(field hpack.HeaderField) {
	if !httpguts.ValidHeaderFieldValue(field.Value) {
		c.invalid = fmt.Errorf("http2: invalid value for header %q", field.Name)
		c.decoder.SetEmitEnabled(false)
		return
	}
	pseudo := field.IsPseudo()
	if pseudo {
		if c.sawRegular {
			c.invalid = errors.New("http2: pseudo-header after regular header")
			c.decoder.SetEmitEnabled(false)
			return
		}
	} else {
		c.sawRegular = true
		if !validWireHeaderName(field.Name) {
			c.invalid = fmt.Errorf("http2: invalid header name %q", field.Name)
			c.decoder.SetEmitEnabled(false)
			return
		}
	}
	fieldSize := uint64(len(field.Name) + len(field.Value) + 32)
	if fieldSize > c.remaining {
		c.remaining = 0
		c.truncated = true
		c.decoder.SetEmitEnabled(false)
		return
	}
	c.remaining -= fieldSize
	c.fields = append(c.fields, field)
}

func (c *headerCodec) resetBlock() {
	c.fields = nil
	c.invalid = nil
}

func validWireHeaderName(name string) bool {
	if !httpguts.ValidHeaderFieldName(name) {
		return false
	}
	for i := range len(name) {
		if name[i] >= 'A' && name[i] <= 'Z' {
			return false
		}
	}
	return true
}
