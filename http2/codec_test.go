package http2

import (
	"bytes"
	"errors"
	"testing"

	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type completeHeaderBlock []byte

func (b completeHeaderBlock) HeaderBlockFragment() []byte { return b }
func (completeHeaderBlock) HeadersEnded() bool            { return true }

func (c *headerCodec) decodeComplete(
	fragment []byte,
	fields []hpack.HeaderField,
) ([]hpack.HeaderField, bool, error, error) {
	return c.decode(nil, 0, completeHeaderBlock(fragment), fields)
}

func TestHeaderCodecConsumesContinuationAfterSemanticError(t *testing.T) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	if err := encoder.WriteField(hpack.HeaderField{Name: "x-before", Value: "before"}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.WriteField(hpack.HeaderField{Name: "X-Invalid", Value: "invalid"}); err != nil {
		t.Fatal(err)
	}
	split := encoded.Len()
	if err := encoder.WriteField(hpack.HeaderField{Name: "x-after", Value: "after"}); err != nil {
		t.Fatal(err)
	}
	block := bytes.Clone(encoded.Bytes())

	var wire bytes.Buffer
	framer := xhttp2.NewFramer(&wire, nil)
	if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: block[:split],
	}); err != nil {
		t.Fatal(err)
	}
	if err := framer.WriteContinuation(1, true, block[split:]); err != nil {
		t.Fatal(err)
	}

	reader := xhttp2.NewFramer(nil, &wire)
	first, err := reader.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
	_, _, invalid, err := codec.decode(reader, 1, first.(*xhttp2.HeadersFrame), nil) //nolint:forcetypeassert
	if err != nil {
		t.Fatalf("decoding semantically invalid block: %v", err)
	}
	if invalid == nil {
		t.Fatal("semantically invalid block was accepted")
	}

	encoded.Reset()
	if err := encoder.WriteField(hpack.HeaderField{Name: "x-after", Value: "after"}); err != nil {
		t.Fatal(err)
	}
	fields, truncated, invalid, err := codec.decodeComplete(bytes.Clone(encoded.Bytes()), nil)
	if err != nil || truncated || invalid != nil {
		t.Fatalf("decoding probe block: fields=%v truncated=%v invalid=%v err=%v", fields, truncated, invalid, err)
	}
	if len(fields) != 1 || fields[0].Name != "x-after" || fields[0].Value != "after" {
		t.Fatalf("probe fields = %#v", fields)
	}
}

func TestHeaderCodecLimitsZeroLengthContinuations(t *testing.T) {
	const continuationLimit = 64
	var wire bytes.Buffer
	writer := xhttp2.NewFramer(&wire, nil)
	if err := writer.WriteHeaders(xhttp2.HeadersFrameParam{StreamID: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= continuationLimit; i++ {
		if err := writer.WriteContinuation(1, i == continuationLimit, nil); err != nil {
			t.Fatal(err)
		}
	}

	reader := xhttp2.NewFramer(nil, &wire)
	first, err := reader.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
	_, _, _, err = codec.decode(reader, 1, first.(*xhttp2.HeadersFrame), nil) //nolint:dogsled,forcetypeassert
	var connectionError xhttp2.ConnectionError
	if !errors.As(err, &connectionError) || xhttp2.ErrCode(connectionError) != xhttp2.ErrCodeEnhanceYourCalm {
		t.Fatalf("decode() error = %v, want ENHANCE_YOUR_CALM connection error", err)
	}
}

func TestHeaderCodecRejectsEdgeWhitespaceValues(t *testing.T) {
	encodeField := func(value string) []byte {
		var encoded bytes.Buffer
		if err := hpack.NewEncoder(&encoded).WriteField(hpack.HeaderField{Name: "x-test", Value: value}); err != nil {
			t.Fatal(err)
		}
		return bytes.Clone(encoded.Bytes())
	}
	for _, value := range []string{" x", "x ", "\tx", "x\t", " "} {
		codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
		_, _, invalid, err := codec.decodeComplete(encodeField(value), nil)
		if err != nil {
			t.Fatalf("value %q: connection error %v", value, err)
		}
		if invalid == nil {
			t.Errorf("value %q with edge whitespace was accepted", value)
		}
	}
	codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
	fields, truncated, invalid, err := codec.decodeComplete(encodeField("a b\tc"), nil)
	if err != nil || truncated || invalid != nil || len(fields) != 1 {
		t.Fatalf("interior whitespace: fields=%v truncated=%v invalid=%v err=%v", fields, truncated, invalid, err)
	}
}

// encodeTestBlock encodes fields with a fresh encoder; a non-nil tableSize
// makes the block begin with that dynamic table size update.
func encodeTestBlock(t testing.TB, tableSize *uint32, fields ...hpack.HeaderField) []byte {
	t.Helper()
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	if tableSize != nil {
		encoder.SetMaxDynamicTableSize(*tableSize)
	}
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	return bytes.Clone(encoded.Bytes())
}

func isCompressionError(err error) bool {
	var connectionError xhttp2.ConnectionError
	return errors.As(err, &connectionError) && xhttp2.ErrCode(connectionError) == xhttp2.ErrCodeCompression
}

// RFC 9113 §4.3.1: once a SETTINGS_HEADER_TABLE_SIZE below the capacity the
// peer declared is acknowledged, its next block must begin with a size update
// to at most the new maximum.
func TestHeaderCodecRequiresSizeUpdateAfterReduction(t *testing.T) {
	size := func(v uint32) *uint32 { return &v }
	field := hpack.HeaderField{Name: "x-test", Value: "value"}
	for _, tc := range []struct {
		name      string
		declared  *uint32 // update in a block before the acknowledgement
		newMax    uint32
		update    *uint32 // update at the start of the block after it
		wantError bool
	}{
		{name: "omitted", newMax: 256, wantError: true},
		{name: "declared", newMax: 256, update: size(256)},
		{name: "declared-smaller", newMax: 256, update: size(0)},
		{name: "declared-too-large", newMax: 256, update: size(4096), wantError: true},
		{name: "zero-maximum", newMax: 0, update: size(0)},
		{name: "zero-maximum-omitted", newMax: 0, wantError: true},
		{name: "not-below-declared", newMax: 4096},
		{name: "below-default-not-below-declared", declared: size(256), newMax: 256},
		{name: "below-declared", declared: size(512), newMax: 256, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
			if _, _, _, err := codec.decodeComplete(encodeTestBlock(t, tc.declared, field), nil); err != nil {
				t.Fatalf("block before the acknowledgement: %v", err)
			}
			codec.setMaxTableSize(tc.newMax)
			_, _, _, err := codec.decodeComplete(encodeTestBlock(t, tc.update, field), nil)
			if tc.wantError != (err != nil) || err != nil && !isCompressionError(err) {
				t.Fatalf("block after the acknowledgement: error = %v, want error %v", err, tc.wantError)
			}
			if tc.wantError {
				return
			}
			// The requirement is for one block only.
			if _, _, _, err := codec.decodeComplete(encodeTestBlock(t, nil, field), nil); err != nil {
				t.Fatalf("next block: %v", err)
			}
		})
	}
}

// The leading size update is found across fragments: its integer may span
// HEADERS and CONTINUATION, and the first fragment may be empty. An empty
// block carries no update.
func TestHeaderCodecFindsSizeUpdateAcrossFragments(t *testing.T) {
	// 1024 takes three bytes: 0x3f 0xe1 0x07.
	update := uint32(1024)
	block := encodeTestBlock(t, &update, hpack.HeaderField{Name: "x-test", Value: "value"})
	if block[0] != 0x3f || block[1] != 0xe1 || block[2] != 0x07 {
		t.Fatalf("block prefix = % x, want the three-byte size update", block[:3])
	}
	for _, tc := range []struct {
		name      string
		split     int
		block     []byte
		wantError bool
	}{
		{name: "integer-split", split: 1, block: block},
		{name: "integer-split-twice", split: 2, block: block},
		{name: "empty-first-fragment", split: 0, block: block},
		{name: "empty-block", split: 0, block: nil, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			writer := xhttp2.NewFramer(&wire, nil)
			if err := writer.WriteHeaders(xhttp2.HeadersFrameParam{StreamID: 1, BlockFragment: tc.block[:tc.split]}); err != nil {
				t.Fatal(err)
			}
			if err := writer.WriteContinuation(1, true, tc.block[tc.split:]); err != nil {
				t.Fatal(err)
			}
			reader := xhttp2.NewFramer(nil, &wire)
			first, err := reader.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
			codec.setMaxTableSize(1024)
			headers, ok := first.(*xhttp2.HeadersFrame)
			if !ok {
				t.Fatalf("first frame is %T", first)
			}
			fields, _, _, err := codec.decode(reader, 1, headers, nil)
			if tc.wantError != (err != nil) || err != nil && !isCompressionError(err) {
				t.Fatalf("decode() error = %v, want error %v", err, tc.wantError)
			}
			if !tc.wantError && (len(fields) != 1 || codec.declaredCapacity != update) {
				t.Fatalf("fields = %v, declared capacity = %d", fields, codec.declaredCapacity)
			}
		})
	}
}

// A size update integer with redundant zero continuation groups is accepted
// by the decoder, so the declared capacity must follow it; otherwise a later
// acknowledged reduction demands an update that isn't due.
func TestHeaderCodecTracksLongSizeUpdateInteger(t *testing.T) {
	codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
	// 31 encoded as 0x3f followed by five zero groups, then :status 200.
	block := []byte{0x3f, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00, 0x88}
	if _, _, _, err := codec.decodeComplete(block, nil); err != nil {
		t.Fatalf("decoding the long size update: %v", err)
	}
	if codec.declaredCapacity != 31 {
		t.Fatalf("declared capacity = %d, want 31", codec.declaredCapacity)
	}
	codec.setMaxTableSize(1024)
	if _, _, _, err := codec.decodeComplete([]byte{0x88}, nil); err != nil {
		t.Fatalf("block after a reduction above the declared capacity: %v", err)
	}
}
