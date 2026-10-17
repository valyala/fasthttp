package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"testing"
)

func TestBoundedStreamDefaultDynamicType(t *testing.T) {
	for _, entry := range []string{"constructor", "response", "request"} {
		t.Run(entry, func(t *testing.T) {
			sw := func(w *bufio.Writer) {
				if _, err := w.WriteString("legacy"); err != nil {
					t.Error(err)
				}
			}
			var r io.Reader
			switch entry {
			case "constructor":
				r = NewStreamReader(sw)
			case "response":
				resp := new(Response)
				resp.SetBodyStreamWriter(sw)
				defer func() {
					if err := resp.CloseBodyStream(); err != nil {
						t.Error(err)
					}
				}()
				r = resp.BodyStream()
			case "request":
				req := new(Request)
				req.SetBodyStreamWriter(sw)
				defer func() {
					if err := req.CloseBodyStream(); err != nil {
						t.Error(err)
					}
				}()
				r = req.BodyStream()
			}
			// The original private PipeConns endpoint implements net.Conn.
			// The opt-in stream does not. No wrapper may replace this type.
			if _, ok := r.(net.Conn); !ok {
				t.Fatalf("default stream dynamic type changed to %T", r)
			}
			if _, ok := r.(BodyWriterTo); ok {
				t.Fatalf("default stream unexpectedly opted into BodyWriterTo: %T", r)
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != "legacy" {
				t.Fatal(string(got), err)
			}
			closer, ok := r.(io.Closer)
			if !ok {
				t.Fatalf("default stream is not closeable: %T", r)
			}
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A caller may override Read even when WriteTo is present; capacity is a
// separate property and only the exact private source selects bounded output.
type boundedSelectionExternal struct {
	*bytes.Reader

	reads int
}

func (r *boundedSelectionExternal) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func (*boundedSelectionExternal) SupportsBodyWriteTo() bool { return false }

type boundedSelectionAllowed struct{ *bytes.Reader }

func (*boundedSelectionAllowed) SupportsBodyWriteTo() bool { return true }

type boundedSelectionWrapped struct {
	io.ReadCloser
	BodyWriterTo
}

func TestBoundedStreamCompressionSelection(t *testing.T) {
	body := streamCompatBody(128*1024 + 17)
	for _, codec := range streamCompatCodecs {
		for _, kind := range []string{"bounded", "legacy", "external-opt-out", "external-opt-in", "wrapped-bounded"} {
			t.Run(codec.name+"/"+kind, func(t *testing.T) {
				sw, done := streamCompatProducer(body, 64*1024, 1)
				var source io.Reader
				var external *boundedSelectionExternal
				switch kind {
				case "bounded":
					source = NewBoundedStreamReader(sw)
				case "legacy":
					source = NewStreamReader(sw)
				case "external-opt-out":
					external = &boundedSelectionExternal{Reader: bytes.NewReader(body)}
					source = external
				case "external-opt-in":
					source = &boundedSelectionAllowed{bytes.NewReader(body)}
				case "wrapped-bounded":
					r := NewBoundedStreamReader(sw)
					wt, ok := r.(BodyWriterTo)
					if !ok {
						t.Fatalf("bounded stream lacks BodyWriterTo: %T", r)
					}
					source = boundedSelectionWrapped{r, wt}
				}
				var resp Response
				resp.Header.SetContentType("text/plain")
				resp.SetBodyStream(source, -1)
				codec.encode(&resp, CompressBestSpeed)
				reader := resp.BodyStream()
				if kind == "bounded" {
					compressed, ok := reader.(*boundedCompressedBodyStream)
					if !ok || compressed.r != nil || compressed.bodyStream != source {
						t.Fatalf("bounded selection/lazy state: %T", reader)
					}
				} else {
					legacy, ok := reader.(*compressedBodyStream)
					if !ok {
						t.Fatalf("default compression type changed: %T", reader)
					}
					if kind == "external-opt-in" {
						// External WriterTo codec data follows the existing path;
						// this case checks exact-type selection and lazy close.
						if err := resp.CloseBodyStream(); err != nil {
							t.Fatal(err)
						}
						if legacy.r != nil {
							t.Fatal("closing unread external source started compression")
						}
						if n, err := reader.Read(nil); n != 0 || err != io.ErrClosedPipe {
							t.Fatal(n, err)
						}
						return
					}
				}
				encoded, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "bounded" {
					if _, ok := reader.(*boundedCompressedBodyStream).r.(*streamPipe); !ok {
						t.Fatal("compressed output is not a bounded bridge")
					}
				} else {
					legacy, ok := reader.(*compressedBodyStream)
					if !ok {
						t.Fatalf("default compression type changed: %T", reader)
					}
					if _, ok := legacy.r.(net.Conn); !ok {
						t.Fatalf("default compressed output type changed: %T", legacy.r)
					}
				}
				if external != nil && external.reads == 0 {
					t.Fatal("external opt-out Read side effects were bypassed")
				}
				if err := resp.CloseBodyStream(); err != nil {
					t.Fatal(err)
				}
				var decoded Response
				decoded.SetBodyRaw(encoded)
				got, err := codec.decode(&decoded)
				if err != nil || !bytes.Equal(got, body) {
					t.Fatal(len(got), err)
				}
				if kind != "external-opt-out" && kind != "external-opt-in" {
					if err := streamCompatAwait(t, done); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
