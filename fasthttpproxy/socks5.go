package fasthttpproxy

import (
	"net"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/proxy"
)

// FasthttpSocksDialer returns a fasthttp.DialFunc that dials using
// the provided SOCKS5 proxy.
//
// Example usage:
// c := &fasthttp.Client{
//   Dial: fasthttpproxy.FasthttpSocksDialer("localhost:9050"),
// }
func FasthttpSocksDialer(proxyAddr string) fasthttp.DialFunc {
	return func(addr string) (net.Conn, error) {
		dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
		if err != nil {
			return nil, err
		}
		return dialer.Dial("tcp", addr)
	}
}
