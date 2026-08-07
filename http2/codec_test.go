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
