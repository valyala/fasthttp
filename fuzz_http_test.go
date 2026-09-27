package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
)

const (
	fuzzBodyLimit    = 64 * 1024
	fuzzNextRequest  = "GET /fuzz-next HTTP/1.1\r\nHost: next.example\r\n\r\n"
	fuzzNextResponse = "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nnext"
)

// Fragment transport reads independently of the bufio buffer and body reads.
// The finite reader also makes an attempted read past a message observable
// without leaving a fuzz worker blocked on an open network connection.
type fuzzFragmentReader struct {
	r *bytes.Reader
	n int
}

func (r *fuzzFragmentReader) Read(p []byte) (int, error) {
	return r.r.Read(p[:min(len(p), r.n)])
}

func fuzzReader(data []byte, fragment uint8) *bufio.Reader {
	return bufio.NewReader(&fuzzFragmentReader{r: bytes.NewReader(data), n: int(fragment) + 1})
}

func fuzzRemaining(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fuzzCheckNextMessage(t *testing.T, br *bufio.Reader, response bool) {
	t.Helper()
	next := fuzzNextRequest
	if response {
		next = fuzzNextResponse
	}
	peek, err := br.Peek(len(next))
	if err != nil || string(peek) != next {
		t.Fatalf("wrong message boundary: next bytes=%q err=%v", peek, err)
	}
	if response {
		var resp Response
		defer resp.Reset()
		if err := resp.ReadLimitBody(br, fuzzBodyLimit); err != nil || resp.StatusCode() != StatusOK || string(resp.Body()) != "next" {
			t.Fatalf("parsing next response: status=%d body=%q err=%v", resp.StatusCode(), resp.Body(), err)
		}
	} else {
		var req Request
		defer req.Reset()
		if err := req.ReadLimitBody(br, fuzzBodyLimit); err != nil || string(req.Header.RequestURI()) != "/fuzz-next" ||
			string(req.Header.Host()) != "next.example" || len(req.Body()) != 0 {
			t.Fatalf("parsing next request: target=%q body=%q err=%v", req.Header.RequestURI(), req.Body(), err)
		}
	}
	if _, err := br.Peek(1); err != io.EOF {
		t.Fatalf("next message left unexpected data: %v", err)
	}
}

func FuzzRequestFraming(f *testing.F) {
	for _, s := range []string{
		"POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 3\r\n\r\nabc",
		"POST / HTTP/1.1\r\nHost: example.com\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\nabc",
		"POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nTrailer: X-Fuzz-Trailer\r\n\r\n3;ext=value\r\nabc\r\n0\r\nX-Fuzz-Trailer: done\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\nContent-Length: 3\r\n\r\nabc",
		"POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 3\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
		"POST / HTTP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
	} {
		f.Add([]byte(s), uint8(7))
	}
	f.Fuzz(func(t *testing.T, data []byte, fragment uint8) {
		if len(data) > fuzzBodyLimit {
			return
		}
		wire := append(bytes.Clone(data), fuzzNextRequest...)
		var previousBody, previousRest []byte
		var previousHeader string
		var previousErr error
		var req Request
		defer req.Reset()
		for pass := range 2 {
			if pass == 1 {
				old := "POST /old?stale=query HTTP/1.1\r\nHost: stale.example\r\nCookie: stale=cookie\r\n" +
					"Content-Type: application/x-www-form-urlencoded\r\nX-Stale: header\r\nContent-Length: 8\r\n\r\nold=form"
				if err := req.ReadLimitBody(fuzzReader([]byte(old), fragment), fuzzBodyLimit); err != nil {
					t.Fatal(err)
				}
				_ = req.URI().QueryArgs()
				_ = req.PostArgs()
				for range req.Header.Cookies() {
				}
			}
			br := fuzzReader(wire, fragment)
			err := req.ReadLimitBody(br, fuzzBodyLimit)
			if err == nil && req.MayContinue() {
				err = req.ContinueReadBody(br, fuzzBodyLimit)
			}
			if req.multipartForm != nil {
				return // Multipart materialization is covered by its dedicated target.
			}
			// Exercise the lazy parsers before reusing the request too.
			_ = req.URI().QueryArgs()
			_ = req.PostArgs()
			for range req.Header.Cookies() {
			}
			body := bytes.Clone(req.Body())
			rest := fuzzRemaining(t, br)
			header := string(req.Header.Header())
			if pass == 1 {
				if (err == nil) != (previousErr == nil) || !bytes.Equal(body, previousBody) ||
					!bytes.Equal(rest, previousRest) || header != previousHeader {
					t.Fatal("reusing request changed parsing")
				}
				break
			}
			previousErr, previousBody, previousRest, previousHeader = err, body, rest, header
			// ReadLimitBody must reset the previous message, including lazy state.
		}
		if previousErr != nil {
			return
		}
		br := fuzzReader(wire, fragment)
		nr, err := http.ReadRequest(br)
		if err != nil {
			return // Acceptance policies intentionally differ; compare common successes.
		}
		defer nr.Body.Close()
		body, err := io.ReadAll(io.LimitReader(nr.Body, fuzzBodyLimit+1))
		if err != nil || len(body) > fuzzBodyLimit {
			return
		}
		if !bytes.Equal(previousBody, body) || !bytes.Equal(previousRest, fuzzRemaining(t, br)) {
			t.Fatal("fasthttp and net/http disagree on request body or message boundary")
		}
	})
}

func FuzzResponseFraming(f *testing.F) {
	for _, s := range []string{
		"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc",
		"HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 204 No Content\r\n\r\n",
		"HTTP/1.1 304 Not Modified\r\nContent-Length: 42\r\n\r\n",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n",
		"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Length: 3\r\n\r\nabc",
	} {
		f.Add([]byte(s), uint8(1), false)
	}
	f.Fuzz(func(t *testing.T, data []byte, fragment uint8, head bool) {
		if len(data) > fuzzBodyLimit {
			return
		}
		wire := append(bytes.Clone(data), fuzzNextResponse...)
		var resp Response
		defer resp.Reset()
		resp.SkipBody = head
		br := fuzzReader(wire, fragment)
		if resp.ReadLimitBody(br, fuzzBodyLimit) != nil {
			return
		}
		body, rest, status := bytes.Clone(resp.Body()), fuzzRemaining(t, br), resp.StatusCode()
		// Populate different state before reuse, as a keep-alive client does.
		resp.SkipBody = false
		old := "HTTP/1.1 201 Created\r\nContent-Type: text/plain\r\nX-Stale: header\r\nContent-Length: 3\r\n\r\nold"
		if err := resp.ReadLimitBody(fuzzReader([]byte(old), fragment), fuzzBodyLimit); err != nil {
			t.Fatal(err)
		}
		resp.SkipBody = head
		br = fuzzReader(wire, fragment)
		if err := resp.ReadLimitBody(br, fuzzBodyLimit); err != nil || status != resp.StatusCode() ||
			!bytes.Equal(body, resp.Body()) || !bytes.Equal(rest, fuzzRemaining(t, br)) {
			t.Fatal("reusing response changed parsing")
		}
		method := MethodGet
		if head {
			method = MethodHead
		}
		br = fuzzReader(wire, fragment)
		var nr *http.Response
		for n := 0; ; n++ {
			var err error
			nr, err = http.ReadResponse(br, &http.Request{Method: method})
			if err != nil {
				return
			}
			if nr.StatusCode < 100 || nr.StatusCode >= 200 || nr.StatusCode == StatusSwitchingProtocols {
				break
			}
			_ = nr.Body.Close()
			if n >= maxInterimResponses {
				return
			}
		}
		defer nr.Body.Close()
		if nr.ProtoMajor != 1 || nr.ProtoMinor > 1 || nr.StatusCode < 100 {
			return // Restrict the oracle to valid HTTP/1.x response semantics.
		}
		other, err := io.ReadAll(io.LimitReader(nr.Body, fuzzBodyLimit+1))
		if err != nil || len(other) > fuzzBodyLimit {
			return
		}
		otherRest := fuzzRemaining(t, br)
		if status != nr.StatusCode || !bytes.Equal(body, other) || !bytes.Equal(rest, otherRest) {
			t.Fatalf("response framing mismatch: fasthttp status=%d body=%q rest=%q; net/http status=%d body=%q rest=%q",
				status, body, rest, nr.StatusCode, other, otherRest)
		}
	})
}

func fuzzChunkedBody(body []byte, chunkSize int) []byte {
	var wire bytes.Buffer
	for len(body) > 0 {
		n := min(len(body), chunkSize)
		fmt.Fprintf(&wire, "%x;fuzz=value\r\n", n)
		wire.Write(body[:n])
		wire.WriteString("\r\n")
		body = body[n:]
	}
	wire.WriteString("0\r\nX-Fuzz-Trailer: done\r\n\r\n")
	return wire.Bytes()
}

func fuzzReadBodyStream(t *testing.T, r io.Reader, readSize uint8) ([]byte, error) {
	t.Helper()
	buf := make([]byte, int(readSize)+1)
	var body []byte
	for range fuzzBodyLimit + 2 {
		n, err := r.Read(buf)
		if n < 0 || n > len(buf) {
			t.Fatalf("invalid Read count: %d", n)
		}
		body = append(body, buf[:n]...)
		if len(body) > fuzzBodyLimit {
			t.Fatal("stream exceeded generated body bound")
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				for range 2 {
					if n, err := r.Read(buf); n != 0 || err != io.EOF {
						t.Fatalf("stream did not stay at EOF: n=%d err=%v", n, err)
					}
				}
				return body, nil
			}
			return body, err
		}
		if n == 0 {
			t.Fatal("stream made no progress on a finite input")
		}
	}
	t.Fatal("stream did not terminate")
	return nil, nil
}

func FuzzBodyStream(f *testing.F) {
	for _, mode := range []uint8{0, 1, 2, 3, 4, 5, 6, 7} {
		f.Add([]byte("hello streamed world"), mode, uint8(1), uint8(3), uint16(1))
	}
	f.Add([]byte{}, uint8(1), uint8(0), uint8(0), uint16(0))
	f.Add([]byte("0"), uint8(5), uint8(7), uint8(25), uint16(70))
	f.Fuzz(func(t *testing.T, body []byte, mode, fragment, readSize uint8, cut uint16) {
		if len(body) > 16*1024 {
			return
		}
		chunked, expect, truncated := mode&1 != 0, mode&2 != 0, mode&4 != 0
		header := fmt.Sprintf("Content-Length: %d\r\n", len(body))
		encoded := bytes.Clone(body)
		if chunked {
			header = "Transfer-Encoding: chunked\r\nTrailer: X-Fuzz-Trailer\r\n"
			encoded = fuzzChunkedBody(body, int(readSize)+1)
		}
		if truncated && len(encoded) > 0 {
			encoded = encoded[:len(encoded)-1-int(cut)%len(encoded)]
		} else {
			truncated = false
		}
		for _, response := range []bool{false, true} {
			firstLine, suffix := "POST /stream HTTP/1.1\r\nHost: example.com\r\n", fuzzNextRequest
			if response {
				firstLine, suffix = "HTTP/1.1 200 OK\r\n", fuzzNextResponse
			} else if expect {
				firstLine += "Expect: 100-continue\r\n"
			}
			wire := append([]byte(firstLine+header+"\r\n"), encoded...)
			if !truncated {
				wire = append(wire, suffix...)
			}
			var req Request
			var resp Response
			for _, streaming := range []bool{false, true} {
				br := fuzzReader(wire, fragment)
				var got []byte
				var err error
				var trailer []byte
				if response {
					resp.Reset()
					resp.StreamBody = streaming
					err = resp.ReadLimitBody(br, fuzzBodyLimit)
					if err == nil && streaming {
						got, err = fuzzReadBodyStream(t, resp.BodyStream(), readSize)
					} else {
						got = bytes.Clone(resp.Body())
					}
					trailer = bytes.Clone(resp.Header.Peek("X-Fuzz-Trailer"))
					resp.Reset()
				} else {
					req.Reset()
					if streaming {
						err = req.Header.Read(br)
						if err == nil {
							err = req.ContinueReadBodyStream(br, int(readSize)+1, false)
						}
						if err == nil {
							got, err = fuzzReadBodyStream(t, req.BodyStream(), readSize)
						}
					} else {
						err = req.ReadLimitBody(br, fuzzBodyLimit)
						if err == nil && req.MayContinue() {
							err = req.ContinueReadBody(br, fuzzBodyLimit)
						}
						got = bytes.Clone(req.Body())
					}
					trailer = bytes.Clone(req.Header.Peek("X-Fuzz-Trailer"))
					req.Reset()
				}
				if truncated {
					// Request streams retain their legacy partial-body EOF behavior.
					if (response || !streaming) && err == nil {
						t.Fatalf("accepted truncated body: response=%v streaming=%v", response, streaming)
					}
					continue
				}
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("body mismatch: response=%v streaming=%v err=%v got=%q want=%q", response, streaming, err, got, body)
				}
				if chunked && string(trailer) != "done" {
					t.Fatalf("lost trailer: %q", trailer)
				}
				fuzzCheckNextMessage(t, br, response)
				if streaming {
					fuzzCloseBodyStream(t, wire, response, fragment, readSize)
				}
			}
		}
	})
}

// Cancel a valid stream after a partial read, then use its object and pooled
// reader for a different body. Closing does not promise to drain the original
// connection, so the next body is supplied by a separate finite reader.
func fuzzCloseBodyStream(t *testing.T, wire []byte, response bool, fragment, readSize uint8) {
	t.Helper()
	var req Request
	var resp Response
	defer req.Reset()
	defer resp.Reset()
	br := fuzzReader(wire, fragment)
	var stream io.Reader
	if response {
		resp.StreamBody = true
		if err := resp.ReadLimitBody(br, fuzzBodyLimit); err != nil {
			t.Fatal(err)
		}
		stream = resp.BodyStream()
	} else {
		if err := req.Header.Read(br); err != nil {
			t.Fatal(err)
		}
		if err := req.ContinueReadBodyStream(br, 1, false); err != nil {
			t.Fatal(err)
		}
		stream = req.BodyStream()
	}
	buf := make([]byte, int(readSize)+1)
	if _, err := stream.Read(buf); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if response {
		if err := resp.CloseBodyStream(); err != nil {
			t.Fatal(err)
		}
		resp.Reset()
		resp.StreamBody = true
		if err := resp.ReadLimitBody(fuzzReader([]byte(fuzzNextResponse), fragment), fuzzBodyLimit); err != nil {
			t.Fatal(err)
		}
		stream = resp.BodyStream()
	} else {
		if err := req.CloseBodyStream(); err != nil {
			t.Fatal(err)
		}
		req.Reset()
		br = fuzzReader([]byte("POST /reset HTTP/1.1\r\nHost: example.com\r\nContent-Length: 4\r\n\r\nnext"), fragment)
		if err := req.Header.Read(br); err != nil {
			t.Fatal(err)
		}
		if err := req.ContinueReadBodyStream(br, 1, false); err != nil {
			t.Fatal(err)
		}
		stream = req.BodyStream()
	}
	got, err := fuzzReadBodyStream(t, stream, readSize)
	if err != nil || string(got) != "next" {
		t.Fatalf("closing/resetting a stream leaked body state: %q err=%v", got, err)
	}
}
