package fasthttpproxy

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"

	"github.com/valyala/fasthttp"
)

// FasthttpHTTPDialer returns a fasthttp.DialFunc that dials using
// the provided HTTP proxy.
//
// Example usage:
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpHTTPDialer("username:password@localhost:9050"),
//	}
func FasthttpHTTPDialer(proxy string) fasthttp.DialFunc {
	var auth string
	if strings.Contains(proxy, "@") {
		split := strings.Split(proxy, "@")
		auth = base64.StdEncoding.EncodeToString([]byte(split[0]))
		proxy = split[1]
	}

	return func(addr string) (net.Conn, error) {
		conn, err := fasthttp.Dial(proxy)
		if err != nil {
			return nil, err
		}

		req := "CONNECT " + addr + " HTTP/1.1\r\n"
		if auth != "" {
			req += "Proxy-Authorization: Basic " + auth + "\r\n"
		}
		req += "\r\n"

		if _, err := conn.Write([]byte(req)); err != nil {
			return nil, err
		}

		res := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(res)

		res.SkipBody = true

		if err := res.Read(bufio.NewReader(conn)); err != nil {
			conn.Close()
			return nil, err
		}
		if res.Header.StatusCode() != 200 {
			conn.Close()
			return nil, fmt.Errorf("could not connect to proxy")
		}
		return conn, nil
	}
}
