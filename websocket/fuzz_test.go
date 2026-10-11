package websocket

import (
	"bytes"
	"context"
	"testing"
	"unicode/utf8"
)

// fuzzConn feeds the fuzz input to a Conn and swallows whatever it writes.
type fuzzConn struct {
	nopConn

	r *bytes.Reader
}

func (c *fuzzConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fuzzConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *fuzzConn) Close() error                { return nil }

func FuzzFrames(f *testing.F) {
	f.Add(appendFrame(nil, opText, true, true, []byte("hello")), false)
	f.Add(appendFrame(appendFrame(nil, opBinary, false, true, []byte{1}), opContinuation, true, true, []byte{2}), false)
	f.Add(appendFrame(appendFrame(nil, opPing, true, true, []byte("p")), opText, true, true, []byte("after ping")), false)
	f.Add(appendFrame(nil, opClose, true, true, []byte{3, 0xe8}), false)
	f.Add(appendFrame(nil, opText, true, false, []byte("unmasked for a client")), true)
	f.Add([]byte{0x80 | opText, 0x80 | 127, 0x80, 0, 0, 0, 0, 0, 0, 0}, false)
	f.Fuzz(func(_ *testing.T, data []byte, client bool) {
		fc := &fuzzConn{r: bytes.NewReader(data)}
		c := newConn(fc, fc, client, "", connConfig{maxMessageSize: 1 << 16})
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				break
			}
		}
		_ = c.CloseNow()
	})
}

func FuzzUTF8State(f *testing.F) {
	f.Add([]byte("héllo"), uint8(2))
	f.Add([]byte{0xe2, 0x82, 0xac}, uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		size := int(chunk%7) + 1
		var s utf8State
		ok := true
		for i := 0; i < len(data) && ok; i += size {
			ok = s.feed(data[i:min(i+size, len(data))])
		}
		if got, want := ok && s.done(), utf8.Valid(data); got != want {
			t.Fatalf("%v chunk %d: got %v, want %v", data, size, got, want)
		}
	})
}
