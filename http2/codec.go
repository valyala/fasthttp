package http2

import (
	"errors"
	"fmt"
	"math"

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
	lower := asciiLower(original)
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

// asciiLower lowers ASCII letters only: a non-ASCII byte is never valid in a
// field name and must reach validation unchanged.
func asciiLower(s string) string {
	for i := range len(s) {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
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

	// declaredCapacity is the dynamic table size the peer's encoder last
	// declared (RFC 7541 §6.3), 4096 until it declares one. After an
	// acknowledged reduction below it, updateRequired demands that the next
	// block begin with a size update of at most requiredUpdateMax (RFC 9113
	// §4.3.1); zero is a valid maximum, hence the flag.
	declaredCapacity  uint32
	requiredUpdateMax uint32
	updateRequired    bool
	prefix            sizeUpdatePrefix
}

// sizeUpdatePrefix parses the dynamic table size updates that lead a block,
// across fragments: the integer of one may span HEADERS and CONTINUATION.
type sizeUpdatePrefix struct {
	done      bool
	sawUpdate bool
	first     uint32
	inInteger bool
	value     uint64
	shift     uint
}

func newHeaderCodec(maxTableSize, maxListSize uint32) *headerCodec {
	codec := &headerCodec{maxListSize: uint64(maxListSize), declaredCapacity: defaultHeaderTableSize}
	codec.decoder = hpack.NewDecoder(maxTableSize, codec.emit)
	codec.decoder.SetAllowedMaxDynamicTableSize(maxTableSize)
	codec.decoder.SetMaxStringLength(int(codec.maxListSize)) // #nosec G115
	return codec
}

// setMaxTableSize applies an acknowledged SETTINGS_HEADER_TABLE_SIZE: the
// peer's updates may not exceed size, and once size is below the capacity
// the peer declared, its next block must begin with an update to at most
// size. The update itself resizes the table and evicts.
func (c *headerCodec) setMaxTableSize(size uint32) {
	c.decoder.SetAllowedMaxDynamicTableSize(size)
	if size < c.declaredCapacity {
		c.updateRequired = true
		c.requiredUpdateMax = size
	}
}

// scanSizeUpdates consumes a fragment's share of the leading size updates and
// reports whether the block may go on.
func (c *headerCodec) scanSizeUpdates(fragment []byte) bool {
	p := &c.prefix
	for _, b := range fragment {
		if p.inInteger {
			// As the decoder reads it (RFC 7541 §5.1): zero continuation
			// groups are accepted; the decoder rejects an integer past 63 bits.
			p.value += uint64(b&0x7f) << p.shift
			if b&0x80 == 0 {
				p.inInteger = false
				c.recordSizeUpdate(p.value)
				continue
			}
			p.shift += 7
			if p.shift >= 63 {
				p.done = true
				return c.checkSizeUpdate()
			}
			continue
		}
		if b&0xe0 != 0x20 {
			p.done = true
			return c.checkSizeUpdate()
		}
		p.value = uint64(b & 0x1f)
		if p.value < 0x1f {
			c.recordSizeUpdate(p.value)
			continue
		}
		p.inInteger = true
		p.shift = 0
	}
	return true
}

func (c *headerCodec) recordSizeUpdate(value uint64) {
	size := uint32(min(value, math.MaxUint32)) // #nosec G115
	c.declaredCapacity = size
	if !c.prefix.sawUpdate {
		c.prefix.sawUpdate = true
		c.prefix.first = size
	}
}

// checkSizeUpdate runs once the block's leading updates are known.
func (c *headerCodec) checkSizeUpdate() bool {
	if !c.updateRequired {
		return true
	}
	c.updateRequired = false
	return c.prefix.sawUpdate && c.prefix.first <= c.requiredUpdateMax
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
	c.prefix = sizeUpdatePrefix{}
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
		if !c.prefix.done && !c.scanSizeUpdates(fragment) {
			decodedFields := c.fields
			c.resetBlock()
			return decodedFields, false, nil, xhttp2.ConnectionError(xhttp2.ErrCodeCompression)
		}
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
	// A block that ends within its prefix, the empty one included.
	if !c.prefix.done && !c.checkSizeUpdate() {
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
	if !httpguts.ValidHeaderFieldValue(field.Value) || hasEdgeWhitespace(field.Value) {
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

// hasEdgeWhitespace reports the leading or trailing SP/HTAB that RFC 9113
// §8.2.1 forbids and httpguts.ValidHeaderFieldValue allows.
func hasEdgeWhitespace(value string) bool {
	if value == "" {
		return false
	}
	first, last := value[0], value[len(value)-1]
	return first == ' ' || first == '\t' || last == ' ' || last == '\t'
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
