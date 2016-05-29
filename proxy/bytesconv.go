package proxy

import (
	"bufio"
	"errors"
	"io"
	"sync"
	"unsafe"
)

// AppendUint appends n to dst and returns the extended dst.
func AppendUint(dst []byte, n int) []byte {
	if n < 0 {
		panic("BUG: int must be positive")
	}

	var b [20]byte
	buf := b[:]
	i := len(buf)
	var q int
	for n >= 10 {
		i--
		q = n / 10
		buf[i] = '0' + byte(n-q*10)
		n = q
	}
	i--
	buf[i] = '0' + byte(n)

	dst = append(dst, buf[i:]...)
	return dst
}

var (
	errEmptyInt            = errors.New("empty integer")
	errUnexpectedFirstChar = errors.New("unexpected first char found. Expecting 0-9")
	//	errUnexpectedTrailingChar = errors.New("unexpected traling char found. Expecting 0-9")
	errTooLongInt = errors.New("too long int")
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
		if i >= maxIntChars {
			return -1, i, errTooLongInt
		}
		v = 10*v + int(k)
	}
	return v, n, nil
}

var (
	errEmptyHexNum    = errors.New("empty hex number")
	errTooLargeHexNum = errors.New("too large hex number")
)

func readHexInt(r *bufio.Reader) (int, error) {
	n := 0
	i := 0
	var k int
	for {
		c, err := r.ReadByte()
		if err != nil {
			if err == io.EOF && i > 0 {
				return n, nil
			}
			return -1, err
		}
		k = hexbyte2int(c)
		if k < 0 {
			if i == 0 {
				return -1, errEmptyHexNum
			}
			r.UnreadByte()
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
		panic("BUG: int must be positive")
	}

	v := hexIntBufPool.Get()
	if v == nil {
		v = make([]byte, maxHexIntChars+1)
	}
	buf := v.([]byte)
	i := len(buf) - 1
	for {
		buf[i] = int2hexbyte(n & 0xf)
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

func int2hexbyte(n int) byte {
	if n < 10 {
		return '0' + byte(n)
	}
	return 'a' + byte(n) - 10
}

func hexCharUpper(c byte) byte {
	if c < 10 {
		return '0' + c
	}
	return c - 10 + 'A'
}

var hex2intTable = func() []byte {
	b := make([]byte, 255)
	for i := byte(0); i < 255; i++ {
		c := byte(0)
		if i >= '0' && i <= '9' {
			c = 1 + i - '0'
		} else if i >= 'a' && i <= 'f' {
			c = 1 + i - 'a' + 10
		} else if i >= 'A' && i <= 'F' {
			c = 1 + i - 'A' + 10
		}
		b[i] = c
	}
	return b
}()

func hexbyte2int(c byte) int {
	return int(hex2intTable[c]) - 1
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

// b2s converts byte slice to a string without memory allocation.
// See https://groups.google.com/forum/#!msg/Golang-Nuts/ENgbUzYvCuU/90yGx7GUAgAJ .
//
// Note it may break if string and/or slice header will change
// in the future go versions.
func b2s(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// AppendQuotedArg appends url-encoded src to dst and returns appended dst.
func AppendQuotedArg(dst, src []byte) []byte {
	for _, c := range src {
		// See http://www.w3.org/TR/html5/forms.html#form-submission-algorithm
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '*' || c == '-' || c == '.' || c == '_' {
			dst = append(dst, c)
		} else {
			dst = append(dst, '%', hexCharUpper(c>>4), hexCharUpper(c&15))
		}
	}
	return dst
}

func appendQuotedPath(dst, src []byte) []byte {
	for _, c := range src {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '/' || c == '.' || c == ',' || c == '=' || c == ':' || c == '&' || c == '~' || c == '-' || c == '_' {
			dst = append(dst, c)
		} else {
			dst = append(dst, '%', hexCharUpper(c>>4), hexCharUpper(c&15))
		}
	}
	return dst
}
