package fasthttpproxy

import (
    "testing"
    "time"
)

// Test generated using Keploy
func TestFasthttpHTTPDialer_ValidProxy(t *testing.T) {
    proxy := "http://localhost:8080"
    dialFunc := FasthttpHTTPDialer(proxy)
    if dialFunc == nil {
        t.Errorf("Expected non-nil DialFunc, got nil")
    }
}

// Test generated using Keploy
func TestFasthttpHTTPDialerDualStack_ValidProxy(t *testing.T) {
    proxy := "http://localhost:8080"
    dialFunc := FasthttpHTTPDialerDualStack(proxy)
    if dialFunc == nil {
        t.Errorf("Expected non-nil DialFunc, got nil")
    }
}

// Test generated using Keploy
func TestFasthttpHTTPDialerTimeout_EmptyProxy(t *testing.T) {
    proxy := ""
    timeout := time.Second * 5
    dialFunc := FasthttpHTTPDialerTimeout(proxy, timeout)
    if dialFunc == nil {
        t.Errorf("Expected non-nil DialFunc, got nil")
    }
}
