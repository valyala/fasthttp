package fasthttpproxy

import (
    "testing"
    "github.com/valyala/fasthttp"
)

// Test generated using Keploy
func TestFasthttpSocksDialer_ValidProxy(t *testing.T) {
    proxyAddr := "socks5://localhost:9050"
    dialFunc := FasthttpSocksDialer(proxyAddr)

    if dialFunc == nil {
        t.Errorf("Expected a valid fasthttp.DialFunc, got nil")
    }

    // Additional check to ensure the returned type is fasthttp.DialFunc
    _, ok := interface{}(dialFunc).(fasthttp.DialFunc)
    if !ok {
        t.Errorf("Expected return type fasthttp.DialFunc, but got a different type")
    }
}


// Test generated using Keploy
func TestFasthttpSocksDialerDualStack_ValidProxy(t *testing.T) {
    proxyAddr := "socks5://localhost:9050"
    dialFunc := FasthttpSocksDialerDualStack(proxyAddr)

    if dialFunc == nil {
        t.Errorf("Expected a valid fasthttp.DialFunc, got nil")
    }

    // Additional check to ensure the returned type is fasthttp.DialFunc
    _, ok := interface{}(dialFunc).(fasthttp.DialFunc)
    if !ok {
        t.Errorf("Expected return type fasthttp.DialFunc, but got a different type")
    }
}
