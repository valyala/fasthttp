package fasthttp

import (
	"bytes"
	"net"
	"testing"
)

// oracleValid replicates the original function's semantics using net.ParseIP:
// - Input must start with '['
// - There must be a closing ']' and a non-empty address between
// - Optional %zone allowed but must not be empty
// - Zone is stripped before checking with net.ParseIP
// - Must contain a ':' to be IPv6 (prevents raw IPv4-in-brackets).
func oracleValid(host []byte) bool {
	if len(host) == 0 || host[0] != '[' {
		// Original function: non-bracketed hosts return nil (treated as valid/no-op).
		return true
	}

	end := bytes.IndexByte(host, ']')
	if end < 0 {
		return false
	}
	addr := host[1:end]
	if len(addr) == 0 {
		return false
	}

	// Split off %zone (if present).
	if zi := bytes.IndexByte(addr, '%'); zi >= 0 {
		// Zone must not be empty.
		if zi == len(addr)-1 {
			return false
		}
		addr = addr[:zi]
	}

	// Must contain ':' to be IPv6.
	if bytes.IndexByte(addr, ':') < 0 {
		return false
	}

	// Use net.ParseIP on the de-zoned address (this was the original check).
	if ip := net.ParseIP(string(addr)); ip == nil {
		return false
	}
	return true
}

func FuzzValidateIPv6Literal(f *testing.F) {
	seeds := [][]byte{
		[]byte(""),            // non-bracketed => valid (no-op)
		[]byte("example.com"), // non-bracketed => valid (no-op)
		[]byte("["),           // unterminated
		[]byte("[]"),          // empty
		[]byte("[::]"),
		[]byte("[::1]"),
		[]byte("[2001:db8::1]"),
		[]byte("[2001:db8::]"),
		[]byte("[::ffff:192.168.0.1]"),
		[]byte("[fe80::1%eth0]"),
		[]byte("[fe80::1%]"),         // empty zone
		[]byte("[1234]"),             // no colon
		[]byte("[2001:db8:zzzz::1]"), // invalid hex
		[]byte("[::ffff:256.0.0.1]"), // invalid v4 tail
		[]byte("[2001:db8:::1]"),     // triple colon
		[]byte("[::1]:443"),          // trailing port outside ']' is ignored by validator
		[]byte("[2001:db8:0:0:0:0:2:1]"),
		[]byte("[2001:db8:0:0:0:0:2:1%en0]"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, host []byte) {
		gotErr := validateIPv6Literal(host)
		wantValid := oracleValid(host)

		if (gotErr == nil) != wantValid {
			t.Fatalf("mismatch for %q: validateIPv6Literal err=%v, oracleValid=%v",
				b2s(host), gotErr, wantValid)
		}
	})
}
