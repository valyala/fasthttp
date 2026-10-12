package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func writeRequestForFramingTest(t *testing.T, req *Request) []byte {
	t.Helper()
	var wire bytes.Buffer
	if _, err := req.WriteTo(&wire); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes()
}

func checkOnlyRequestOnWire(t *testing.T, wire, wantBody []byte) *http.Request {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(wire))
	parsed, err := http.ReadRequest(r)
	if err != nil {
		t.Fatalf("parse request from %q: %v", wire, err)
	}
	gotBody, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("body = %q, want %q", gotBody, wantBody)
	}
	if _, err = http.ReadRequest(r); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected request after body: %v; wire = %q", err, wire)
	}
	return parsed
}

func TestRequestWriteRawContentLengthMatchesBody(t *testing.T) {
	t.Parallel()
	body := []byte("body")
	for _, tc := range []struct {
		name   string
		key    string
		value  string
		method string
		size   int
	}{
		{name: "lowercase", key: "content-length", value: "1"},
		{name: "canonical", key: "Content-Length", value: "1"},
		{name: "mixed-case", key: "cOnTeNt-LeNgTh", value: "1"},
		{name: "invalid", key: "Content-Length", value: "garbage"},
		{name: "missing"},
		{name: "bodyless", key: "Content-Length", value: "100", method: MethodGet},
		{name: "fixed stream", key: "Content-Length", value: "1", size: 4},
		{name: "long stream", key: "Content-Length", value: "1", size: 1},
		{name: "unknown stream", size: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req Request
			req.Header.DisableSpecialHeader()
			req.Header.DisableNormalizing()
			req.Header.SetMethod(MethodPost)
			req.Header.Set("Host", "example.com")
			if tc.key != "" {
				req.Header.Set(tc.key, tc.value)
			}
			wantBody := body
			wantLength := int64(len(body))
			switch {
			case tc.method == MethodGet:
				req.Header.SetMethod(MethodGet)
				wantBody, wantLength = nil, 0
			case tc.size != 0:
				req.SetBodyStream(io.LimitReader(bytes.NewReader(body), int64(len(body))), tc.size)
				if tc.size < 0 {
					wantLength = -1
				} else {
					wantBody, wantLength = body[:tc.size], int64(tc.size)
				}
			default:
				req.SetBodyRaw(body)
			}
			var wire bytes.Buffer
			_, err := req.WriteTo(&wire)
			if tc.size > 0 && tc.size < len(body) {
				if !errors.Is(err, errBodyStreamTooLong) {
					t.Fatalf("write error = %v, want oversized stream error", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			parsed := checkOnlyRequestOnWire(t, wire.Bytes(), wantBody)
			if parsed.ContentLength != wantLength {
				t.Fatalf("Content-Length = %d, want %d", parsed.ContentLength, wantLength)
			}
			if tc.key != "" && !bytes.Contains(wire.Bytes(), []byte(tc.key+": ")) {
				t.Fatalf("raw field casing was lost: %q", wire.Bytes())
			}
		})
	}
}

func TestRequestWriteRejectsAmbiguousRawFraming(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		keys   []string
		stream bool
		want   error
	}{
		{"duplicate lengths", []string{"Content-Length", "content-length"}, false, ErrDuplicateContentLength},
		{"length and encoding", []string{"Content-Length", "transfer-encoding"}, false, errConflictingRequestFraming},
		{"fixed body with encoding", []string{"transfer-encoding"}, false, ErrUnsupportedTransferEncoding},
		{"duplicate encodings", []string{"Transfer-Encoding", "transfer-encoding"}, false, ErrUnsupportedTransferEncoding},
		{"unknown stream with length", []string{"Content-Length"}, true, errConflictingRequestFraming},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req Request
			req.Header.DisableSpecialHeader()
			req.Header.DisableNormalizing()
			req.Header.SetMethod(MethodPost)
			req.Header.Set("Host", "example.com")
			for _, key := range tc.keys {
				value := "1"
				if strings.EqualFold(key, HeaderTransferEncoding) {
					value = "chunked"
				}
				req.Header.Add(key, value)
			}
			if tc.stream {
				req.SetBodyStream(strings.NewReader("body"), -1)
			} else {
				req.SetBodyString("body")
			}
			var wire bytes.Buffer
			w := bufio.NewWriter(&wire)
			err := req.Write(w)
			if !errors.Is(err, tc.want) || wire.Len() != 0 || w.Buffered() != 0 {
				t.Fatalf("Write() = %v, wire = %q; want %v and no bytes", err, wire.Bytes(), tc.want)
			}
		})
	}
}

func TestRequestWriteRawChunkedFraming(t *testing.T) {
	t.Parallel()
	var req Request
	req.Header.DisableSpecialHeader()
	req.Header.DisableNormalizing()
	req.Header.SetMethod(MethodPost)
	req.Header.Set("Host", "example.com")
	req.Header.Set("transfer-encoding", "chunked")
	if err := req.Header.SetTrailer("X-Trailer"); err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Trailer", "done")
	req.SetBodyString("body")
	wire := writeRequestForFramingTest(t, &req)
	if bytes.Count(wire, []byte("transfer-encoding: chunked\r\n")) != 1 {
		t.Fatalf("chunked framing field missing or duplicated: %q", wire)
	}
	parsed := checkOnlyRequestOnWire(t, wire, []byte("body"))
	if got := parsed.Trailer.Get("X-Trailer"); got != "done" {
		t.Fatalf("trailer = %q, want done", got)
	}
}

type bodyReadFromBuffer struct {
	bytes.Buffer

	source io.Reader
	limit  int64
	calls  int
}

func (w *bodyReadFromBuffer) ReadFrom(r io.Reader) (int64, error) {
	w.calls++
	if lr, ok := r.(*io.LimitedReader); ok {
		w.source = lr.R
		w.limit = lr.N
	} else {
		w.source = r
		w.limit = -1
	}
	return w.Buffer.ReadFrom(r)
}

func TestWriteBodyFixedSizeFileKeepsReadFrom(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("X"), maxSmallFileSize+1)
	for _, tc := range []struct {
		name      string
		fileRange bool
	}{
		{name: "file-grown"},
		{name: "fs-range", fileRange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/body"
			fileBody := append(bytes.Clone(body), 'Y')
			if err := os.WriteFile(path, fileBody, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var stream io.Reader = f
			if tc.fileRange {
				stream = &bigFileReader{f: f, r: io.LimitReader(f, int64(len(body)))}
			}
			var sink bodyReadFromBuffer
			w := bufio.NewWriter(&sink)
			if _, err := w.WriteString("header"); err != nil {
				t.Fatal(err)
			}
			var bw *fixedSizeBodyWriter
			err = writeBodyFixedSize(w, stream, int64(len(body)), &bw)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if sink.calls != 1 || sink.source != f || sink.limit != int64(len(body)) ||
				!bytes.Equal(sink.Bytes(), append([]byte("header"), body...)) {
				t.Fatalf("ReadFrom calls=%d, source=%T, limit=%d, written=%d", sink.calls, sink.source, sink.limit, sink.Len())
			}
		})
	}
}

func TestRequestReadRawLowercaseFraming(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		framing string
		body    string
	}{
		{"fixed", "content-length: 4", "body"},
		{"chunked", "transfer-encoding: chunked", "4\r\nbody\r\n0\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req Request
			req.Header.DisableSpecialHeader()
			req.Header.DisableNormalizing()
			r := bufio.NewReader(strings.NewReader("POST /one HTTP/1.1\r\nHost: example.com\r\n" +
				tc.framing + "\r\n\r\n" + tc.body + "GET /two HTTP/1.1\r\nHost: example.com\r\n\r\n"))
			if err := req.Read(r); err != nil {
				t.Fatal(err)
			}
			if got := string(req.Body()); got != "body" {
				t.Fatalf("first body = %q, want body", got)
			}
			if err := req.Read(r); err != nil {
				t.Fatal(err)
			}
			if got := string(req.Header.RequestURI()); got != "/two" {
				t.Fatalf("next request URI = %q, want /two", got)
			}
		})
	}
}
