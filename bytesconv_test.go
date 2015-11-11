package fasthttp

import (
	"testing"
	"time"
)

func TestAppendHTTPDate(t *testing.T) {
	d := time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)
	s := string(AppendHTTPDate(nil, d))
	expectedS := "Tue, 10 Nov 2009 23:00:00 GMT"
	if s != expectedS {
		t.Fatalf("unexpected date %q. Expecting %q", s, expectedS)
	}

	b := []byte("prefix")
	s = string(AppendHTTPDate(b, d))
	if s[:len(b)] != string(b) {
		t.Fatalf("unexpected prefix %q. Expecting %q", s[:len(b)], b)
	}
	s = s[len(b):]
	if s != expectedS {
		t.Fatalf("unexpected date %q. Expecting %q", s, expectedS)
	}
}

func TestParseUintSuccess(t *testing.T) {
	testParseUintSuccess(t, "0", 0)
	testParseUintSuccess(t, "123", 123)
	testParseUintSuccess(t, "123456789012345678", 123456789012345678)
}

func TestParseUintError(t *testing.T) {
	// empty string
	testParseUintError(t, "")

	// negative value
	testParseUintError(t, "-123")

	// non-num
	testParseUintError(t, "foobar234")

	// non-num chars at the end
	testParseUintError(t, "123w")

	// floating point num
	testParseUintError(t, "1234.545")

	// too big num
	testParseUintError(t, "12345678901234567890")
}

func TestParseUfloatSuccess(t *testing.T) {
	testParseUfloatSuccess(t, "0", 0)
	testParseUfloatSuccess(t, "1.", 1.)
	testParseUfloatSuccess(t, ".1", 0.1)
	testParseUfloatSuccess(t, "123.456", 123.456)
	testParseUfloatSuccess(t, "123", 123)
	testParseUfloatSuccess(t, "1234e2", 1234e2)
	testParseUfloatSuccess(t, "1234E-5", 1234E-5)
	testParseUfloatSuccess(t, "1.234e+3", 1.234e+3)
}

func TestParseUfloatError(t *testing.T) {
	// empty num
	testParseUfloatError(t, "")

	// negative num
	testParseUfloatError(t, "-123.53")

	// non-num chars
	testParseUfloatError(t, "123sdfsd")
	testParseUfloatError(t, "sdsf234")
	testParseUfloatError(t, "sdfdf")

	// non-num chars in exponent
	testParseUfloatError(t, "123e3s")
	testParseUfloatError(t, "12.3e-op")
	testParseUfloatError(t, "123E+SS5")

	// duplicate point
	testParseUfloatError(t, "1.3.4")

	// duplicate exponent
	testParseUfloatError(t, "123e5e6")

	// missing exponent
	testParseUfloatError(t, "123534e")
}

func testParseUfloatError(t *testing.T, s string) {
	n, err := ParseUfloat([]byte(s))
	if err == nil {
		t.Fatalf("Expecting error when parsing %q. obtained %f", s, n)
	}
	if n >= 0 {
		t.Fatalf("Expecting negative num instead of %f when parsing %q", n, s)
	}
}

func testParseUfloatSuccess(t *testing.T, s string, expectedF float64) {
	f, err := ParseUfloat([]byte(s))
	if err != nil {
		t.Fatalf("Unexpected error when parsing %q: %s", s, err)
	}
	delta := f - expectedF
	if delta < 0 {
		delta = -delta
	}
	if delta > expectedF*1e-10 {
		t.Fatalf("Unexpected value when parsing %q: %f. Expected %f", s, f, expectedF)
	}
}

func testParseUintError(t *testing.T, s string) {
	n, err := ParseUint([]byte(s))
	if err == nil {
		t.Fatalf("Expecting error when parsing %q. obtained %d", s, n)
	}
	if n >= 0 {
		t.Fatalf("Unexpected n=%d when parsing %q. Expected negative num", n, s)
	}
}

func testParseUintSuccess(t *testing.T, s string, expectedN int) {
	n, err := ParseUint([]byte(s))
	if err != nil {
		t.Fatalf("Unexpected error when parsing %q: %s", s, err)
	}
	if n != expectedN {
		t.Fatalf("Unexpected value %d. Expected %d. num=%q", n, expectedN, s)
	}
}
