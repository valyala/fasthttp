package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"testing"
)

func FuzzMultipartFraming(f *testing.F) {
	for _, mode := range []uint8{0, 1, 2, 3, 4, 5, 8, 9, 10, 11, 16, 17} {
		f.Add([]byte("form value"), "fuzz-boundary", []byte("epilogue"), mode, uint8(0))
	}
	f.Add(bytes.Repeat([]byte("x"), 9000), "file-boundary", bytes.Repeat([]byte("e"), 5000), uint8(0), uint8(255))
	f.Add([]byte("--0"), "0", []byte("0"), uint8('Q'), uint8(14))
	f.Fuzz(func(t *testing.T, value []byte, boundary string, epilogue []byte, mode, fragment uint8) {
		if len(value) > 16*1024 || len(epilogue) > 8192 || len(boundary) > 70 {
			return
		}
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		if mw.SetBoundary(boundary) != nil {
			return
		}
		if err := mw.WriteField("field", string(value)); err != nil {
			t.Fatal(err)
		}
		fw, err := mw.CreateFormFile("file", "fuzz.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(value); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		// A delimiter can occur at the start of a part or after CRLF within
		// its value. Such forms are malformed, so don't assert their fields.
		checkValues := !bytes.HasPrefix(value, []byte("--"+boundary)) &&
			!bytes.Contains(value, []byte("\r\n--"+boundary))
		body := append(bytes.Clone(form.Bytes()), epilogue...)
		contentType := mw.FormDataContentType()
		gzip, chunked, truncated := mode&1 != 0, mode&2 != 0, mode&4 != 0
		if mode&16 != 0 && checkValues {
			fuzzMultipartLimit(t, body, contentType, gzip, int(fragment)+1)
		}
		if gzip {
			body = AppendGzipBytesLevel(nil, body, CompressBestSpeed)
		}
		header := fmt.Sprintf("Content-Type: %s\r\nContent-Length: %d\r\n", contentType, len(body))
		if chunked {
			header = fmt.Sprintf("Content-Type: %s\r\nTransfer-Encoding: chunked\r\nTrailer: X-Fuzz-Trailer\r\n", contentType)
			body = fuzzChunkedBody(body, 127)
		}
		if gzip {
			header += "Content-Encoding: gzip\r\n"
		}
		if mode&8 != 0 {
			// Leave a complete form followed by a truncated declared epilogue.
			// The MIME closing delimiter cannot override HTTP Content-Length.
			header = fmt.Sprintf("Content-Type: %s\r\nContent-Length: %d\r\n", contentType, len(form.Bytes())+len(epilogue)+100)
			body = append(bytes.Clone(form.Bytes()), epilogue...)
			truncated = true
		} else if truncated {
			body = body[:len(body)/2]
		}
		wire := append([]byte("POST /form HTTP/1.1\r\nHost: example.com\r\n"+header+"\r\n"), body...)
		if !truncated {
			wire = append(wire, fuzzNextRequest...)
		}
		for _, streaming := range []bool{false, true} {
			for _, preparse := range []bool{false, true} {
				fuzzMultipartRead(t, wire, value, streaming, preparse, truncated, checkValues, fragment)
			}
		}
	})
}

func fuzzMultipartLimit(t *testing.T, body []byte, contentType string, gzip bool, limit int) {
	t.Helper()
	var req Request
	defer req.Reset()
	req.Header.SetContentType(contentType)
	if gzip {
		req.Header.SetContentEncoding("gzip")
		req.SetBody(AppendGzipBytesLevel(nil, body, CompressBestSpeed))
	} else {
		req.SetBody(body)
	}
	_, err := req.MultipartFormWithLimit(limit)
	if len(body) > limit {
		if !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("multipart limit: expected ErrBodyTooLarge, got %v", err)
		}
	} else if err != nil {
		t.Fatalf("multipart form within limit failed: %v", err)
	}
}

func fuzzMultipartRead(t *testing.T, wire, value []byte, streaming, preparse, truncated, checkValues bool, fragment uint8) {
	t.Helper()
	var req Request
	defer req.Reset() // Also removes multipart files on every failure path.
	br := fuzzReader(wire, fragment)
	err := req.Header.Read(br)
	if err != nil {
		t.Fatal(err)
	}
	if streaming {
		err = req.ContinueReadBodyStream(br, 1024, preparse)
	} else {
		err = req.ContinueReadBody(br, fuzzBodyLimit, preparse)
	}
	if truncated && !streaming {
		if err == nil {
			t.Fatalf("multipart reader accepted truncated HTTP body: preparse=%v", preparse)
		}
		return
	}
	if err != nil {
		if truncated || !checkValues {
			return
		}
		t.Fatalf("reading multipart body: streaming=%v preparse=%v: %v", streaming, preparse, err)
	}
	mf, formErr := req.MultipartFormWithLimit(fuzzBodyLimit)
	if !truncated && checkValues {
		if formErr != nil {
			t.Fatalf("parsing multipart form: %v", formErr)
		}
		if len(mf.Value["field"]) != 1 || mf.Value["field"][0] != string(value) || len(mf.File["file"]) != 1 {
			t.Fatal("multipart fields changed")
		}
		file, err := mf.File["file"][0].Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(file)
		closeErr := file.Close()
		if err != nil || closeErr != nil || !bytes.Equal(got, value) {
			t.Fatalf("multipart file changed: read=%v close=%v", err, closeErr)
		}
	}
	// The handler is done with the form, but epilogue bytes can remain in a
	// streaming body. Drain that body before reusing the connection.
	if req.IsBodyStream() {
		_, err = io.Copy(io.Discard, req.BodyStream())
		if err != nil && !truncated && checkValues {
			t.Fatal(err)
		}
	}
	if truncated || formErr != nil {
		return
	}
	fuzzCheckNextMessage(t, br, false)
}
