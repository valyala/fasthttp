package fasthttpproxy

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"
)

var (
	// Used for caching authentication information when using an HTTP proxy,
	// it helps avoid re-encoding the authentication details when the ProxyURL
	// changes along with the request URL.
	authCache    = sync.Map{}
	colonTLSPort = ":443"
	tmpURL       = &url.URL{Scheme: httpsScheme, Host: "example.com"}
)

// Dialer embeds both fasthttp.TCPDialer and httpproxy.Config, allowing it
// to take advantage of the optimizations provided by fasthttp for dialing while also
// utilizing the finer-grained configuration options offered by httpproxy.
type Dialer struct {
	fasthttp.TCPDialer
	// Support HTTPProxy, HTTPSProxy and NoProxy configuration.
	//
	// HTTPProxy represents the value of the HTTP_PROXY or
	// http_proxy environment variable. It will be used as the proxy
	// URL for HTTP requests unless overridden by NoProxy.
	//
	// HTTPSProxy represents the HTTPS_PROXY or https_proxy
	// environment variable. It will be used as the proxy URL for
	// HTTPS requests unless overridden by NoProxy.
	//
	// NoProxy represents the NO_PROXY or no_proxy environment
	// variable. It specifies a string that contains comma-separated values
	// specifying hosts that should be excluded from proxying. Each value is
	// represented by an IP address prefix (1.2.3.4), an IP address prefix in
	// CIDR notation (1.2.3.4/8), a domain name, or a special DNS label (*).
	// An IP address prefix and domain name can also include a literal port
	// number (1.2.3.4:80).
	// A domain name matches that name and all subdomains. A domain name with
	// a leading "." matches subdomains only. For example "foo.com" matches
	// "foo.com" and "bar.foo.com"; ".y.com" matches "x.y.com" but not "y.com".
	// A single asterisk (*) indicates that no proxying should be done.
	// A best effort is made to parse the string and errors are
	// ignored.
	httpproxy.Config
	// Attempt to connect to both ipv4 and ipv6 addresses if set to true.
	// By default, dial only to ipv4 addresses,
	// since unfortunately ipv6 remains broken in many networks worldwide :)
	//
	// This field from the fasthttp client is provided redundantly here because
	// when we customize the Dial function for the client, its DialDualStack field
	// configuration becomes ineffective.
	DialDualStack bool
	// Dial timeout.
	//
	// This field from the fasthttp client is provided redundantly here because
	// when we customize the Dial function for the client, its DialTimeout field
	// configuration becomes ineffective.
	Timeout time.Duration
	// The timeout for sending a CONNECT request when using an HTTP proxy.
	ConnectTimeout time.Duration
}

// GetDialFunc method returns a fasthttp-style dial function. The useEnv parameter
// determines whether the proxy address comes from Dialer.Config or from environment variables.
func (d *Dialer) GetDialFunc(useEnv bool) (dialFunc fasthttp.DialFunc, err error) {
	config := &d.Config
	if useEnv {
		config = httpproxy.FromEnvironment()
	}
	proxyURLIsSame := config.HTTPSProxy == config.HTTPProxy && config.NoProxy == ""
	network := "tcp4"
	if d.DialDualStack {
		network = "tcp"
	}
	proxyFunc := config.ProxyFunc()
	if proxyURLIsSame {
		var proxyURL *url.URL
		var proxyDialer proxy.Dialer
		proxyURL, err = proxyFunc(tmpURL)
		if err != nil {
			return nil, err
		}
		if proxyURL == nil {
			// dial directly
			return func(addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}, nil
		}
		switch proxyURL.Scheme {
		case "socks5", "socks5h":
			proxyDialer, err = proxy.FromURL(proxyURL, d)
			if err != nil {
				return
			}
		case "http":
			proxyAddr, auth := addrAndAuth(proxyURL)
			proxyDialer = DialerFunc(func(network, addr string) (conn net.Conn, err error) {
				return httpProxyDial(d, network, addr, proxyAddr, auth)
			})
		default:
			return nil, errors.New("proxy: unknown scheme: " + proxyURL.Scheme)
		}
		return func(addr string) (net.Conn, error) {
			return proxyDialer.Dial(network, addr)
		}, nil
	}
	// slow path when the proxyURL changes along with the request URL.
	return func(addr string) (conn net.Conn, err error) {
		var proxyDialer proxy.Dialer
		var proxyURL *url.URL
		scheme := httpsScheme
		if !strings.HasSuffix(addr, colonTLSPort) {
			scheme = httpScheme
		}
		reqURL := &url.URL{Host: addr, Scheme: scheme}
		proxyURL, err = proxyFunc(reqURL)
		if err != nil {
			return
		}
		if proxyURL == nil {
			// dial directly
			return d.Dial(network, addr)
		}
		switch proxyURL.Scheme {
		case "socks5", "socks5h":
			proxyDialer, err = proxy.FromURL(proxyURL, d)
			if err != nil {
				return
			}
		case "http":
			proxyAddr, auth := addrAndAuth(proxyURL)
			proxyDialer = DialerFunc(func(network, addr string) (conn net.Conn, err error) {
				return httpProxyDial(d, network, addr, proxyAddr, auth)
			})
		default:
			return nil, errors.New("proxy: unknown scheme: " + proxyURL.Scheme)
		}
		return proxyDialer.Dial(network, addr)
	}, nil
}

// Dial is solely for implementing the proxy.Dialer interface.
func (d *Dialer) Dial(network, addr string) (conn net.Conn, err error) {
	if network == "tcp4" {
		if d.Timeout > 0 {
			return d.DialTimeout(addr, d.Timeout)
		}
		return d.TCPDialer.Dial(addr)
	}
	if network == "tcp" {
		if d.Timeout > 0 {
			return d.DialDualStackTimeout(addr, d.Timeout)
		}
		return d.TCPDialer.DialDualStack(addr)
	}
	err = errors.New("dont support the network: " + network)
	return
}

func (d *Dialer) connectTimeout() time.Duration {
	return d.ConnectTimeout
}

// In the httpProxyDial function, the proxy.Dialer that implements
// this interface can retrieve timeout information when sending the CONNECT
// method to the HTTP proxy.
type httpProxyDialer interface {
	connectTimeout() time.Duration
}

// DialerFunc Make a function of type func(network, addr string) (net.Conn, error)
// implement the proxy.Dialer interface.
type DialerFunc func(network, addr string) (net.Conn, error)

func (d DialerFunc) Dial(network, addr string) (net.Conn, error) {
	return d(network, addr)
}

// Establish a connection through an HTTP proxy.
func httpProxyDial(dialer proxy.Dialer, network, addr, proxyAddr, auth string) (conn net.Conn, err error) {
	conn, err = dialer.Dial(network, proxyAddr)
	if err != nil {
		return
	}
	var connectTimeout time.Duration
	hp, ok := dialer.(httpProxyDialer)
	if ok {
		connectTimeout = hp.connectTimeout()
	}

	if connectTimeout > 0 {
		if err = conn.SetDeadline(time.Now().Add(connectTimeout)); err != nil {
			_ = conn.Close()
			return nil, err
		}
		defer func() {
			_ = conn.SetDeadline(time.Time{})
		}()
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	req += "\r\n"
	_, err = conn.Write([]byte(req))
	if err != nil {
		_ = conn.Close()
		return
	}
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)
	res.SkipBody = true
	if err = res.Read(bufio.NewReaderSize(conn, 1024)); err != nil {
		_ = conn.Close()
		return
	}
	if res.Header.StatusCode() != 200 {
		_ = conn.Close()
		err = fmt.Errorf("could not connect to proxyAddr: %s status code: %d", proxyAddr, res.Header.StatusCode())
		return
	}
	return
}

// Cache authentication information for HTTP proxies.
type proxyInfo struct {
	auth string
	addr string
}

func addrAndAuth(pu *url.URL) (proxyAddr, auth string) {
	if pu.User == nil {
		proxyAddr = pu.Host + pu.Path
		return
	}
	var info *proxyInfo
	v, ok := authCache.Load(pu)
	if ok {
		info = v.(*proxyInfo)
		return info.addr, info.auth
	}
	info = &proxyInfo{
		auth: base64.StdEncoding.EncodeToString([]byte(pu.User.String())),
		addr: pu.Host + pu.Path,
	}
	authCache.Store(pu, info)
	return info.addr, info.auth
}
