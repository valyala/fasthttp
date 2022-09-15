//go:build !windows
// +build !windows

package fasthttp

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// See issue #1232
func TestRstConnResponseWhileSending(t *testing.T) {
	const expectedStatus = http.StatusTeapot
	const payload = "payload"

	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	go func() {
		for {
			conn, err := srv.Accept()
			if err != nil {
				return
			}

			// Read at least one byte of the header
			// Otherwise we would have an unsolicited response
			_, err = io.ReadAll(io.LimitReader(conn, 1))
			if err != nil {
				t.Error(err)
			}

			// Respond
			_, err = conn.Write([]byte("HTTP/1.1 418 Teapot\r\n\r\n"))
			if err != nil {
				t.Error(err)
			}

			// Forcefully close connection
			err = conn.(*net.TCPConn).SetLinger(0)
			if err != nil {
				t.Error(err)
			}
			conn.Close()
		}
	}()

	svrUrl := "http://" + srv.Addr().String()
	client := HostClient{Addr: srv.Addr().String()}

	for i := 0; i < 100; i++ {
		req := AcquireRequest()
		defer ReleaseRequest(req)
		resp := AcquireResponse()
		defer ReleaseResponse(resp)

		req.Header.SetMethod("POST")
		req.SetBodyStream(strings.NewReader(payload), len(payload))
		req.SetRequestURI(svrUrl)

		err = client.Do(req, resp)
		if err != nil {
			t.Fatal(err)
		}
		if expectedStatus != resp.StatusCode() {
			t.Fatalf("Expected %d status code, but got %d", expectedStatus, resp.StatusCode())
		}
	}
}

// See issue #1232
func TestRstConnClosedWithoutResponse(t *testing.T) {
	const payload = "payload"

	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	go func() {
		for {
			conn, err := srv.Accept()
			if err != nil {
				return
			}

			// Read at least one byte of the header
			// Otherwise we would have an unsolicited response
			_, err = io.ReadAll(io.LimitReader(conn, 1))
			if err != nil {
				t.Error(err)
			}

			// Respond with incomplete header
			_, err = conn.Write([]byte("Http"))
			if err != nil {
				t.Error(err)
			}

			// Forcefully close connection
			err = conn.(*net.TCPConn).SetLinger(0)
			if err != nil {
				t.Error(err)
			}
			conn.Close()
		}
	}()

	svrUrl := "http://" + srv.Addr().String()
	client := HostClient{Addr: srv.Addr().String()}

	for i := 0; i < 100; i++ {
		req := AcquireRequest()
		defer ReleaseRequest(req)
		resp := AcquireResponse()
		defer ReleaseResponse(resp)

		req.Header.SetMethod("POST")
		req.SetBodyStream(strings.NewReader(payload), len(payload))
		req.SetRequestURI(svrUrl)

		err = client.Do(req, resp)

		if !isConnectionReset(err) {
			t.Fatal("Expected connection reset error")
		}
	}
}
