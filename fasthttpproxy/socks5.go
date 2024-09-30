package fasthttpproxy

import (
	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
)

// FasthttpSocksDialer returns a fasthttp.DialFunc that dials using
// the provided SOCKS5 proxy.
//
// Example usage:
//
//	c := &fasthttp.Client{
//		Dial: fasthttpproxy.FasthttpSocksDialer("socks5://localhost:9050"),
//	}
func FasthttpSocksDialer(proxyAddr string) fasthttp.DialFunc {
	d := Dialer{Config: httpproxy.Config{HTTPProxy: proxyAddr, HTTPSProxy: proxyAddr}}
	dialFunc, _ := d.GetDialFunc(false)
	return dialFunc
}
