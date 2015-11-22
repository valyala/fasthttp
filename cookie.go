package fasthttp

import (
	"bytes"
	"errors"
	"io"
	"time"
)

// CookieExpireDelete may be set on Cookie.Expire for expiring the given cookie.
var CookieExpireDelete = time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)

// Cookie represents HTTP response cookie.
type Cookie struct {
	// Key is cookie name.
	Key []byte

	// Value is cookie value.
	Value []byte

	// Expiration time for the cookie.
	//
	// Set expiration time to CookieExpireDelete for expiring (deleting)
	// the cookie on the client.
	//
	// By default cookie lifetime is limited by browser session.
	Expire time.Time

	// Domain for the cookie.
	//
	// By default cookie is set to the current domain.
	Domain []byte

	// Path for the cookie.
	//
	// By default cookie is set to the current page only.
	Path []byte

	bufKV argsKV
	buf   []byte
}

var zeroTime time.Time

// Reset clears the cookie.
func (c *Cookie) Reset() {
	c.Key = c.Key[:0]
	c.Value = c.Value[:0]
	c.Expire = zeroTime
	c.Domain = c.Domain[:0]
	c.Path = c.Path[:0]
}

// AppendBytes appends cookie representation to dst and returns dst
// (maybe newly allocated).
func (c *Cookie) AppendBytes(dst []byte) []byte {
	if len(c.Key) > 0 {
		dst = appendQuotedArg(dst, c.Key)
		dst = append(dst, '=')
	}
	dst = appendQuotedArg(dst, c.Value)

	if !c.Expire.IsZero() {
		c.bufKV.value = AppendHTTPDate(c.bufKV.value[:0], c.Expire)
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieExpires...)
		dst = append(dst, '=')
		dst = append(dst, c.bufKV.value...)
	}
	if len(c.Domain) > 0 {
		dst = appendCookiePart(dst, strCookieDomain, c.Domain)
	}
	if len(c.Path) > 0 {
		dst = appendCookiePart(dst, strCookiePath, c.Path)
	}
	return dst
}

// Cookie returns cookie representation.
//
// The returned value is valid until the next call to Cookie methods.
func (c *Cookie) Cookie() []byte {
	c.buf = c.AppendBytes(c.buf[:0])
	return c.buf
}

// String returns cookie representation.
func (c *Cookie) String() string {
	return string(c.Cookie())
}

// WriteTo writes cookie representation to w.
//
// WriteTo implements io.WriterTo interface.
func (c *Cookie) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(c.Cookie())
	return int64(n), err
}

var errNoCookies = errors.New("no cookies found")

// Parse parses Set-Cookie header.
func (c *Cookie) Parse(src string) error {
	c.buf = AppendBytesStr(c.buf[:0], src)
	return c.ParseBytes(c.buf)
}

// ParseBytes parses Set-Cookie header.
func (c *Cookie) ParseBytes(src []byte) error {
	c.Reset()

	var s cookieScanner
	s.b = src

	kv := &c.bufKV
	if !s.next(kv, true) {
		return errNoCookies
	}

	c.Key = append(c.Key[:0], kv.key...)
	c.Value = append(c.Value[:0], kv.value...)

	for s.next(kv, false) {
		if len(kv.key) == 0 && len(kv.value) == 0 {
			continue
		}
		switch {
		case bytes.Equal(strCookieExpires, kv.key):
			v := unsafeBytesToStr(kv.value)
			exptime, err := time.ParseInLocation(time.RFC1123, v, gmtLocation)
			if err != nil {
				return err
			}
			c.Expire = exptime
		case bytes.Equal(strCookieDomain, kv.key):
			c.Domain = append(c.Domain[:0], kv.value...)
		case bytes.Equal(strCookiePath, kv.key):
			c.Path = append(c.Path[:0], kv.value...)
		}
	}
	return nil
}

func appendCookiePart(dst, key, value []byte) []byte {
	dst = append(dst, ';', ' ')
	dst = append(dst, key...)
	dst = append(dst, '=')
	return append(dst, value...)
}

func getCookieKey(dst, src []byte) []byte {
	n := bytes.IndexByte(src, '=')
	if n >= 0 {
		src = src[:n]
	}
	return decodeCookieArg(dst, src, true)
}

func appendRequestCookieBytes(dst []byte, cookies []argsKV) []byte {
	for i, n := 0, len(cookies); i < n; i++ {
		kv := &cookies[i]
		if len(kv.key) > 0 {
			dst = appendQuotedArg(dst, kv.key)
			dst = append(dst, '=')
		}
		dst = appendQuotedArg(dst, kv.value)
		if i+1 < n {
			dst = append(dst, ';', ' ')
		}
	}
	return dst
}

func parseRequestCookies(cookies []argsKV, src []byte) []argsKV {
	var s cookieScanner
	s.b = src
	var kv *argsKV
	cookies, kv = allocArg(cookies)
	for s.next(kv, true) {
		if len(kv.key) > 0 || len(kv.value) > 0 {
			cookies, kv = allocArg(cookies)
		}
	}
	return releaseArg(cookies)
}

type cookieScanner struct {
	b []byte
}

func (s *cookieScanner) next(kv *argsKV, decode bool) bool {
	if len(s.b) == 0 {
		return false
	}

	isKey := true
	k := 0
	for i, c := range s.b {
		switch c {
		case '=':
			if isKey {
				isKey = false
				kv.key = decodeCookieArg(kv.key, s.b[:i], decode)
				k = i + 1
			}
		case ';':
			if isKey {
				kv.key = kv.key[:0]
			}
			kv.value = decodeCookieArg(kv.value, s.b[k:i], decode)
			s.b = s.b[i+1:]
			return true
		}
	}

	if isKey {
		kv.key = kv.key[:0]
	}
	kv.value = decodeCookieArg(kv.value, s.b[k:], decode)
	s.b = s.b[len(s.b):]
	return true
}

func decodeCookieArg(dst, src []byte, decode bool) []byte {
	for len(src) > 0 && src[0] == ' ' {
		src = src[1:]
	}
	for len(src) > 0 && src[len(src)-1] == ' ' {
		src = src[:len(src)-1]
	}
	if !decode {
		return append(dst[:0], src...)
	}
	return decodeArg(dst, src, true)
}
