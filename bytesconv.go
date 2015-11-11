package fasthttp

import (
	"bufio"
	"fmt"
	"math"
	"sync"
	"time"
	"unsafe"
)

var (
	maxIntChars = func() int {
		switch ^uint(0) {
		case 0xffffffff:
			// 32 bit
			return 9
		case 0xffffffffffffffff:
			// 64 bit
			return 18
		default:
			panic("Unsupported architecture :)")
		}
	}()

	maxHexIntChars = func() int {
		switch ^uint(0) {
		case 0xffffffff:
			// 32 bit
			return 7
		case 0xffffffffffffffff:
			// 64 bit
			return 15
		default:
			panic("Unsupported architecture :)")
		}
	}()

	gmtLocation = func() *time.Location {
		x, err := time.LoadLocation("GMT")
		if err != nil {
			panic(fmt.Sprintf("cannot load GMT location: %s", err))
		}
		return x
	}()
)

// AppendHTTPDate appends HTTP-compliant (RFC1123) representation of date
// to dst and returns dst (which may be newly allocated).
func AppendHTTPDate(dst []byte, date time.Time) []byte {
	return date.In(gmtLocation).AppendFormat(dst, time.RFC1123)
}

// ParseUint parses uint from buf.
func ParseUint(buf []byte) (int, error) {
	v, n, err := parseUintBuf(buf)
	if n != len(buf) {
		return -1, fmt.Errorf("only %b bytes out of %d bytes exhausted when parsing int %q", n, len(buf), buf)
	}
	return v, err
}

func parseUintBuf(b []byte) (int, int, error) {
	n := len(b)
	if n == 0 {
		return -1, 0, fmt.Errorf("empty integer")
	}
	v := 0
	for i := 0; i < n; i++ {
		c := b[i]
		k := c - '0'
		if k > 9 {
			if i == 0 {
				return -1, i, fmt.Errorf("unexpected first char %c. Expected 0-9", c)
			}
			return v, i, nil
		}
		if i >= maxIntChars {
			return -1, i, fmt.Errorf("too long int %q", b[:i+1])
		}
		v = 10*v + int(k)
	}
	return v, n, nil
}

// ParseUfloat parses unsigned float from buf.
func ParseUfloat(buf []byte) (float64, error) {
	if len(buf) == 0 {
		return -1, fmt.Errorf("empty float number")
	}
	b := buf
	var v uint64
	var offset float64 = 1.0
	var pointFound bool
	for i, c := range b {
		if c < '0' || c > '9' {
			if c == '.' {
				if pointFound {
					return -1, fmt.Errorf("duplicate point found in %q", buf)
				}
				pointFound = true
				continue
			}
			if c == 'e' || c == 'E' {
				if i+1 >= len(b) {
					return -1, fmt.Errorf("unexpected end of float after %c. num=%q", c, buf)
				}
				b = b[i+1:]
				minus := -1
				switch b[0] {
				case '+':
					b = b[1:]
					minus = 1
				case '-':
					b = b[1:]
				default:
					minus = 1
				}
				vv, err := ParseUint(b)
				if err != nil {
					return -1, fmt.Errorf("cannot parse exponent part of %q: %s", buf, err)
				}
				return float64(v) * offset * math.Pow10(minus*int(vv)), nil
			}
			return -1, fmt.Errorf("unexpected char found %c in %q", c, buf)
		}
		v = 10*v + uint64(c-'0')
		if pointFound {
			offset /= 10
		}
	}
	return float64(v) * offset, nil
}

func readHexInt(r *bufio.Reader) (int, error) {
	n := 0
	i := 0
	var k byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return -1, err
		}
		if c >= '0' && c <= '9' {
			k = c - '0'
		} else if c >= 'a' && c <= 'f' {
			k = 10 + c - 'a'
		} else if c >= 'A' && c <= 'F' {
			k = 10 + c - 'A'
		} else {
			if i == 0 {
				return -1, fmt.Errorf("cannot read hex num from empty string")
			}
			r.UnreadByte()
			return n, nil
		}
		if i >= maxHexIntChars {
			return -1, fmt.Errorf("cannot read hex num with more than %d digits", maxHexIntChars)
		}
		n = n*16 + int(k)
		i++
	}
}

var intBufPool sync.Pool

func writeInt(w *bufio.Writer, n int) error {
	if n < 0 {
		panic("BUG: int must be positive")
	}

	v := intBufPool.Get()
	if v == nil {
		v = make([]byte, maxIntChars+8)
	}
	buf := v.([]byte)
	i := len(buf) - 1
	for {
		v := byte(n % 10)
		n /= 10
		buf[i] = '0' + v
		if n == 0 {
			break
		}
		i--
	}
	_, err := w.Write(buf[i:])
	intBufPool.Put(v)
	return err
}

const toLower = 'a' - 'A'

func uppercaseByte(p *byte) {
	c := *p
	if c >= 'a' && c <= 'z' {
		*p = c - toLower
	}
}

func lowercaseByte(p *byte) {
	c := *p
	if c >= 'A' && c <= 'Z' {
		*p = c + toLower
	}
}

func lowercaseBytes(b []byte) {
	for i, n := 0, len(b); i < n; i++ {
		lowercaseByte(&b[i])
	}
}

// Converts byte slice to a string without memory allocation.
// See https://groups.google.com/forum/#!msg/Golang-Nuts/ENgbUzYvCuU/90yGx7GUAgAJ .
//
// Note it may break if string and/or slice header will change
// in the future go versions.
func unsafeBytesToStr(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}
