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
				r = resp.BodyStream()
			case "request":
				req := new(Request)
				req.SetBodyStreamWriter(sw)
				r = req.BodyStream()
			}
			// Legacy entry points retain the PipeConns endpoint and do not opt in.
			conn, ok := r.(net.Conn)
			if !ok {
				t.Fatalf("legacy stream type changed: %T", r)
			}
			defer streamClose(t, conn)
			if _, ok := r.(BodyWriterTo); ok {
				t.Fatalf("legacy stream opted into BodyWriterTo: %T", r)
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != "legacy" {
				t.Fatal(string(got), err)
			}
		})
	}
}

type boundedSelectionExternal struct{ *bytes.Reader }

func (*boundedSelectionExternal) SupportsBodyWriteTo() bool { return false }

type boundedSelectionAllowed struct{ *bytes.Reader }

func (*boundedSelectionAllowed) SupportsBodyWriteTo() bool { return true }

type boundedSelectionWrapped struct {
	io.ReadCloser
	BodyWriterTo
}

func TestBoundedStreamCompressionSelection(t *testing.T) {
	// This tests the selector added by this patch, not existing codec dispatch.
	// In particular, SupportsBodyWriteTo says nothing about a byte-capacity bound.
	for _, kind := range []string{"bounded", "legacy", "external-opt-out", "external-opt-in", "wrapped-bounded"} {
		t.Run(kind, func(t *testing.T) {
			var source io.Reader
			switch kind {
			case "bounded":
				source = NewBoundedStreamReader(func(*bufio.Writer) {})
			case "legacy":
				source = NewStreamReader(func(*bufio.Writer) {})
			case "external-opt-out":
				source = &boundedSelectionExternal{bytes.NewReader(nil)}
			case "external-opt-in":
				source = &boundedSelectionAllowed{bytes.NewReader(nil)}
			case "wrapped-bounded":
				r := NewBoundedStreamReader(func(*bufio.Writer) {})
				wt, ok := r.(BodyWriterTo)
				if !ok {
					t.Fatalf("bounded reader lacks BodyWriterTo: %T", r)
				}
				source = boundedSelectionWrapped{r, wt}
			}
			started := false
			reader := newCompressedBodyStream(source, 0, func(*bufio.Writer, io.Reader, int) error { started = true; return nil })
			_, bounded := reader.(*boundedCompressedBodyStream)
			if bounded != (kind == "bounded") {
				t.Fatalf("%s selected %T", kind, reader)
			}
			if !bounded {
				if _, ok := reader.(*compressedBodyStream); !ok {
					t.Fatalf("legacy compression type changed: %T", reader)
				}
			}
			streamClose(t, reader)
			if started {
				t.Fatal("closing unread stream started compression")
			}
			if n, err := reader.Read(nil); n != 0 || err != io.ErrClosedPipe {
				t.Fatal(n, err)
			}
		})
	}
}
