//go:generate go run bytesconv_table_gen.go

package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// AppendHTMLEscape appends html-escaped s to dst and returns the extended dst.
func AppendHTMLEscape(dst []byte, s string) []byte {
	var (
		prev int
		sub  string
	)

	for i, n := 0, len(s); i < n; i++ {
		sub = ""
		switch s[i] {
		case '&':
			sub = "&amp;"
		case '<':
			sub = "&lt;"
		case '>':
			sub = "&gt;"
		case '"':
			sub = "&#34;" // "&#34;" is shorter than "&quot;".
		case '\'':
			sub = "&#39;" // "&#39;" is shorter than "&apos;" and apos was not in HTML until HTML5.
		}
		if sub != "" {
			dst = append(dst, s[prev:i]...)
			dst = append(dst, sub...)
			prev = i + 1
		}
	}
	return append(dst, s[prev:]...)
}

// AppendHTMLEscapeBytes appends html-escaped s to dst and returns
// the extended dst.
func AppendHTMLEscapeBytes(dst, s []byte) []byte {
	return AppendHTMLEscape(dst, b2s(s))
}

// AppendIPv4 appends string representation of the given ip v4 to dst
// and returns the extended dst.
func AppendIPv4(dst []byte, ip net.IP) []byte {
	ip = ip.To4()
	if ip == nil {
		return append(dst, "non-v4 ip passed to AppendIPv4"...)
	}

	dst = AppendUint(dst, int(ip[0]))
	for i := 1; i < 4; i++ {
		dst = append(dst, '.')
		dst = AppendUint(dst, int(ip[i]))
	}
	return dst
}

var errEmptyIPStr = errors.New("empty ip address string")

var httpDateGMT = time.FixedZone("GMT", 0)

// ParseIPv4 parses ip address from ipStr into dst and returns the extended dst.
func ParseIPv4(dst net.IP, ipStr []byte) (net.IP, error) {
	if len(ipStr) == 0 {
		return dst, errEmptyIPStr
	}
	if len(dst) < net.IPv4len || len(dst) > net.IPv4len {
		dst = make([]byte, net.IPv4len)
	}
	copy(dst, net.IPv4zero)
	dst = dst.To4() // dst is always non-nil here

	b := ipStr
	for i := 0; i < 3; i++ {
		n := bytes.IndexByte(b, '.')
		if n < 0 {
			return dst, fmt.Errorf("cannot find dot in ipStr %q", ipStr)
		}
		v, err := ParseUint(b[:n])
		if err != nil {
			return dst, fmt.Errorf("cannot parse ipStr %q: %w", ipStr, err)
		}
		if v > 255 {
			return dst, fmt.Errorf("cannot parse ipStr %q: ip part cannot exceed 255: parsed %d", ipStr, v)
		}
		dst[i] = byte(v)
		b = b[n+1:]
	}
	v, err := ParseUint(b)
	if err != nil {
		return dst, fmt.Errorf("cannot parse ipStr %q: %w", ipStr, err)
	}
	if v > 255 {
		return dst, fmt.Errorf("cannot parse ipStr %q: ip part cannot exceed 255: parsed %d", ipStr, v)
	}
	dst[3] = byte(v)

	return dst, nil
}

// AppendHTTPDate appends HTTP-compliant (RFC1123) representation of date
// to dst and returns the extended dst.
func AppendHTTPDate(dst []byte, date time.Time) []byte {
	dst = date.In(time.UTC).AppendFormat(dst, time.RFC1123)
	copy(dst[len(dst)-3:], strGMT)
	return dst
}

// ParseHTTPDate parses HTTP-compliant (RFC1123) date.
func ParseHTTPDate(date []byte) (time.Time, error) {
	if t, ok := parseRFC1123DateGMT(date); ok {
		return t, nil
	}
	return time.Parse(time.RFC1123, b2s(date))
}

func parseRFC1123DateGMT(b []byte) (time.Time, bool) {
	// Expects "Mon, 02 Jan 2006 15:04:05 GMT".
	if len(b) != 29 {
		return time.Time{}, false
	}
	if !isWeekday3(b[0], b[1], b[2]) {
		return time.Time{}, false
	}
	if b[3] != ',' || b[4] != ' ' || b[7] != ' ' || b[11] != ' ' ||
		b[16] != ' ' || b[19] != ':' || b[22] != ':' || b[25] != ' ' {
		return time.Time{}, false
	}
	if (b[26]|0x20) != 'g' || (b[27]|0x20) != 'm' || (b[28]|0x20) != 't' {
		return time.Time{}, false
	}

	day, ok := parse2Digits(b[5], b[6])
	if !ok || day < 1 || day > 31 {
		return time.Time{}, false
	}
	month, ok := parseMonth3(b[8], b[9], b[10])
	if !ok {
		return time.Time{}, false
	}
	year, ok := parse4Digits(b[12], b[13], b[14], b[15])
	if !ok {
		return time.Time{}, false
	}
	hour, ok := parse2Digits(b[17], b[18])
	if !ok || hour > 23 {
		return time.Time{}, false
	}
	minute, ok := parse2Digits(b[20], b[21])
	if !ok || minute > 59 {
		return time.Time{}, false
	}
	second, ok := parse2Digits(b[23], b[24])
	if !ok || second > 59 {
		return time.Time{}, false
	}

	t := time.Date(year, month, day, hour, minute, second, 0, httpDateGMT)
	// Reject calendar-invalid dates like "31 Feb", which time.Date normalizes.
	if t.Year() != year || t.Month() != month || t.Day() != day {
		return time.Time{}, false
	}
	return t, true
}

func isWeekday3(a, b, c byte) bool {
	a |= 0x20
	b |= 0x20
	c |= 0x20
	k := uint32(a)<<16 | uint32(b)<<8 | uint32(c)
	switch k {
	case uint32('m')<<16 | uint32('o')<<8 | uint32('n'),
		uint32('t')<<16 | uint32('u')<<8 | uint32('e'),
		uint32('w')<<16 | uint32('e')<<8 | uint32('d'),
		uint32('t')<<16 | uint32('h')<<8 | uint32('u'),
		uint32('f')<<16 | uint32('r')<<8 | uint32('i'),
		uint32('s')<<16 | uint32('a')<<8 | uint32('t'),
		uint32('s')<<16 | uint32('u')<<8 | uint32('n'):
		return true
	default:
		return false
	}
}

func parse2Digits(a, b byte) (int, bool) {
	if a < '0' || a > '9' || b < '0' || b > '9' {
		return 0, false
	}
	return int(a-'0')*10 + int(b-'0'), true
}

func parse4Digits(a, b, c, d byte) (int, bool) {
	v1, ok := parse2Digits(a, b)
	if !ok {
		return 0, false
	}
	v2, ok := parse2Digits(c, d)
	if !ok {
		return 0, false
	}
	return v1*100 + v2, true
}

func parseMonth3(a, b, c byte) (time.Month, bool) {
	a |= 0x20
	b |= 0x20
	c |= 0x20
	k := uint32(a)<<16 | uint32(b)<<8 | uint32(c)
	switch k {
	case uint32('j')<<16 | uint32('a')<<8 | uint32('n'):
		return time.January, true
	case uint32('f')<<16 | uint32('e')<<8 | uint32('b'):
		return time.February, true
	case uint32('m')<<16 | uint32('a')<<8 | uint32('r'):
		return time.March, true
	case uint32('a')<<16 | uint32('p')<<8 | uint32('r'):
		return time.April, true
	case uint32('m')<<16 | uint32('a')<<8 | uint32('y'):
		return time.May, true
	case uint32('j')<<16 | uint32('u')<<8 | uint32('n'):
		return time.June, true
	case uint32('j')<<16 | uint32('u')<<8 | uint32('l'):
		return time.July, true
	case uint32('a')<<16 | uint32('u')<<8 | uint32('g'):
		return time.August, true
	case uint32('s')<<16 | uint32('e')<<8 | uint32('p'):
		return time.September, true
	case uint32('o')<<16 | uint32('c')<<8 | uint32('t'):
		return time.October, true
	case uint32('n')<<16 | uint32('o')<<8 | uint32('v'):
		return time.November, true
	case uint32('d')<<16 | uint32('e')<<8 | uint32('c'):
		return time.December, true
	}
	return 0, false
}

// AppendUint appends n to dst and returns the extended dst.
func AppendUint(dst []byte, n int) []byte {
	if n < 0 {
		// developer sanity-check
		panic("BUG: int must be positive")
	}

	return strconv.AppendUint(dst, uint64(n), 10)
}

// ParseUint parses uint from buf.
func ParseUint(buf []byte) (int, error) {
	v, n, err := parseUintBuf(buf)
	if n != len(buf) {
		return -1, errUnexpectedTrailingChar
	}
	return v, err
}

var (
	errEmptyInt               = errors.New("empty integer")
	errUnexpectedFirstChar    = errors.New("unexpected first char found. Expecting 0-9")
	errUnexpectedTrailingChar = errors.New("unexpected trailing char found. Expecting 0-9")
	errTooLongInt             = errors.New("too long int")
)

func parseUintBuf(b []byte) (int, int, error) {
	n := len(b)
	if n == 0 {
		return -1, 0, errEmptyInt
	}
	v := 0
	for i := 0; i < n; i++ {
		c := b[i]
		k := c - '0'
		if k > 9 {
			if i == 0 {
				return -1, i, errUnexpectedFirstChar
			}
			return v, i, nil
		}
		vNew := 10*v + int(k)
		// Test for overflow.
		if vNew < v {
			return -1, i, errTooLongInt
		}
		v = vNew
	}
	return v, n, nil
}

// ParseUfloat parses unsigned float from buf.
func ParseUfloat(buf []byte) (float64, error) {
	// The implementation of parsing a float string is not easy.
	// We believe that the conservative approach is to call strconv.ParseFloat.
	// https://github.com/valyala/fasthttp/pull/1865
	res, err := strconv.ParseFloat(b2s(buf), 64)
	if res < 0 {
		return -1, errors.New("negative input is invalid")
	}
	if err != nil {
		return -1, err
	}
	return res, err
}

var (
	errEmptyHexNum    = errors.New("empty hex number")
	errTooLargeHexNum = errors.New("too large hex number")
)

func readHexInt(r *bufio.Reader) (int, error) {
	var k, i, n int
	for {
		c, err := r.ReadByte()
		if err != nil {
			if err == io.EOF && i > 0 {
				return n, nil
			}
			return -1, err
		}
		k = int(hex2intTable[c])
		if k == 16 {
			if i == 0 {
				return -1, errEmptyHexNum
			}
			if err := r.UnreadByte(); err != nil {
				return -1, err
			}
			return n, nil
		}
		if i >= maxHexIntChars {
			return -1, errTooLargeHexNum
		}
		n = (n << 4) | k
		i++
	}
}

var hexIntBufPool sync.Pool

func writeHexInt(w *bufio.Writer, n int) error {
	if n < 0 {
		// developer sanity-check
		panic("BUG: int must be positive")
	}

	v := hexIntBufPool.Get()
	if v == nil {
		v = make([]byte, maxHexIntChars+1)
	}
	buf := v.([]byte)
	i := len(buf) - 1
	for {
		buf[i] = lowerhex[n&0xf]
		n >>= 4
		if n == 0 {
			break
		}
		i--
	}
	_, err := w.Write(buf[i:])
	hexIntBufPool.Put(v)
	return err
}

const (
	upperhex = "0123456789ABCDEF"
	lowerhex = "0123456789abcdef"
)

func lowercaseBytes(b []byte) {
	for i := 0; i < len(b); i++ {
		p := &b[i]
		*p = toLowerTable[*p]
	}
}

// AppendUnquotedArg appends url-decoded src to dst and returns appended dst.
//
// dst may point to src. In this case src will be overwritten.
func AppendUnquotedArg(dst, src []byte) []byte {
	return decodeArgAppend(dst, src)
}

// AppendQuotedArg appends url-encoded src to dst and returns appended dst.
func AppendQuotedArg(dst, src []byte) []byte {
	for _, c := range src {
		switch {
		case c == ' ':
			dst = append(dst, '+')
		case quotedArgShouldEscapeTable[int(c)] != 0:
			dst = append(dst, '%', upperhex[c>>4], upperhex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

func appendQuotedPath(dst, src []byte) []byte {
	// Fix issue in https://github.com/golang/go/issues/11202
	if len(src) == 1 && src[0] == '*' {
		return append(dst, '*')
	}

	for _, c := range src {
		if quotedPathShouldEscapeTable[int(c)] != 0 {
			dst = append(dst, '%', upperhex[c>>4], upperhex[c&0xf])
		} else {
			dst = append(dst, c)
		}
	}
	return dst
}
