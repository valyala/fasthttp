package fasthttpproxy

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/valyala/fasthttp"
)

// FasthttpProxyHTTPDialer returns a fasthttp.DialFunc that dials using
// the the env(HTTP_PROXY, HTTPS_PROXY and NO_PROXY) configured HTTP proxy.
//
// Example usage:
//	c := &fasthttp.Client{
//		Dial: FasthttpProxyHTTPDialer(),
//	}
func FasthttpProxyHTTPDialer() fasthttp.DialFunc {
	return FasthttpProxyHTTPDialerTimeout(0)
}

// FasthttpProxyHTTPDialer returns a fasthttp.DialFunc that dials using
// the env(HTTP_PROXY, HTTPS_PROXY and NO_PROXY) configured HTTP proxy using the given timeout.
//
// Example usage:
//	c := &fasthttp.Client{
//		Dial: FasthttpProxyHTTPDialerTimeout(time.Second * 2),
//	}
func FasthttpProxyHTTPDialerTimeout(timeout time.Duration) fasthttp.DialFunc {
	proxier := httpproxy.FromEnvironment().ProxyFunc()

	// map on proxy URL and its encoded auth barrier
	authBarriers := map[*url.URL]string{}
	authBarriersLock := sync.RWMutex{}

	return func(addr string) (net.Conn, error) {

		proxyURL, err := proxier(&url.URL{Host: addr})
		if err != nil {
			return nil, err
		}

		if proxyURL == nil {
			if timeout == 0 {
				return fasthttp.Dial(addr)
			}
			return fasthttp.DialTimeout(addr, timeout)
		}

		var conn net.Conn
		if timeout == 0 {
			conn, err = fasthttp.Dial(proxyURL.String())
		} else {
			conn, err = fasthttp.DialTimeout(proxyURL.String(), timeout)
		}
		if err != nil {
			return nil, err
		}

		req := "CONNECT " + addr + " HTTP/1.1\r\n"
		if proxyURL.User != nil {
			authBarriersLock.RLock()
			barrier, ok := authBarriers[proxyURL]
			authBarriersLock.RUnlock()

			if !ok {
				barrier = base64.StdEncoding.EncodeToString([]byte(proxyURL.User.String()))
				authBarriersLock.Lock()
				authBarriers[proxyURL] = barrier
				authBarriersLock.Unlock()
			}

			req += "Proxy-Authorization: Basic " + barrier + "\r\n"
		}
		req += "\r\n"

		if _, err := conn.Write([]byte(req)); err != nil {
			return nil, err
		}

		res := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseResponse(res)

		res.SkipBody = true

		if err := res.Read(bufio.NewReader(conn)); err != nil {
			if connErr := conn.Close(); connErr != nil {
				return nil, fmt.Errorf("conn close err %v followed by read conn err %w", connErr, err)
			}
			return nil, err
		}
		if res.Header.StatusCode() != 200 {
			if connErr := conn.Close(); connErr != nil {
				return nil, fmt.Errorf(
					"conn close err %v followed by connect to proxy: code: %d body %s",
					connErr, res.StatusCode(), string(res.Body()))
			}
			return nil, fmt.Errorf("could not connect to proxy: code: %d body %s", res.StatusCode(), string(res.Body()))
		}
		return conn, nil
	}
}
