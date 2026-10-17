package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestStreamCompressedReadWriteToResume(t *testing.T) {
	body := streamCompatBody(128*1024 + 17)
	for _, codec := range streamCompatCodecs {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial=%t", codec.name, partial), func(t *testing.T) {
				sw, done := streamCompatProducer(body, 64*1024, 1)
				var resp Response
				resp.Header.SetContentType("text/plain")
				resp.SetBodyStream(NewBoundedStreamReader(sw), -1)
				codec.encode(&resp, CompressBestSpeed)
				defer func() {
					if err := resp.CloseBodyStream(); err != nil {
						t.Error(err)
					}
				}()
				reader := resp.BodyStream()
				wt, ok := reader.(BodyWriterTo)
				if !ok || !wt.SupportsBodyWriteTo() {
					t.Fatal("compressed bounded stream lost its explicit WriteTo capability")
				}
				prefix := make([]byte, 7)
				if _, err := io.ReadFull(reader, prefix); err != nil {
					t.Fatal(err)
				}
				var encoded bytes.Buffer
				encoded.Write(prefix)
				if partial {
					sentinel := errors.New("compressed destination failed")
					dst := streamAuditBoundaryWriter{failAt: 1, n: 13, err: sentinel}
					n, err := wt.WriteTo(&dst)
					if !errors.Is(err, sentinel) || n != int64(dst.dst.Len()) || n != 13 {
						t.Fatalf("encoded accepted count=%d,%v, destination bytes=%d", n, err, dst.dst.Len())
					}
					encoded.Write(dst.dst.Bytes())
					// Read must resume the same encoded stream and retained output
					// prefix, without starting another codec/header after failure.
					tail, err := io.ReadAll(reader)
					if err != nil {
						t.Fatal(err)
					}
					encoded.Write(tail)
				} else {
					before := encoded.Len()
					n, err := wt.WriteTo(&encoded)
					if err != nil || n != int64(encoded.Len()-before) {
						t.Fatalf("encoded WriteTo count=%d,%v; actual=%d", n, err, encoded.Len()-before)
					}
				}
				var extra bytes.Buffer
				if n, err := wt.WriteTo(&extra); n != 0 || err != nil || extra.Len() != 0 {
					t.Fatalf("repeated WriteTo after EOF=%d,%v, bytes=%d", n, err, extra.Len())
				}
				var decoded Response
				decoded.SetBodyRaw(encoded.Bytes())
				got, err := codec.decode(&decoded)
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("resumed compressed body len=%d,err=%v; expected %d", len(got), err, len(body))
				}
				if err := streamCompatAwait(t, done); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
