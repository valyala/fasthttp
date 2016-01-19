package fasthttp

import (
	"bytes"
	"errors"
	"io"
	"time"
)

var zeroTime time.Time

var (
	// CookieExpireDelete may be set on Cookie.Expire for expiring the given cookie.
	CookieExpireDelete = time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)

	// CookieExpireUnlimited indicates that the cookie doesn't expire.
	CookieExpireUnlimited = zeroTime
)

// Cookie represents HTTP response cookie.
//
// Do not copy Cookie objects. Create new object and use CopyTo instead.
//
// It is unsafe modifying/reading Cookie instance from concurrently
// running goroutines.
type Cookie struct {
	key    []byte
	value  []byte
	expire time.Time
	domain []byte
	path   []byte

	bufKV argsKV
	buf   []byte
}

// CopyTo copies src cookie to c.
func (c *Cookie) CopyTo(src *Cookie) {
	c.Reset()
	c.key = append(c.key[:0], src.key...)
	c.value = append(c.value[:0], src.value...)
	c.expire = src.expire
	c.domain = append(c.domain[:0], src.domain...)
	c.path = append(c.path[:0], src.path...)
}

// Path returns cookie path.
func (c *Cookie) Path() []byte {
	return c.path
}

// SetPath sets cookie path.
func (c *Cookie) SetPath(path string) {
	c.buf = append(c.buf[:0], path...)
	c.path = normalizePath(c.path, c.buf)
}

// SetPathBytes sets cookie path.
func (c *Cookie) SetPathBytes(path []byte) {
	c.buf = append(c.buf[:0], path...)
	c.path = normalizePath(c.path, c.buf)
}

// Domain returns cookie domain.
//
// The returned domain is valid until the next Cookie modification method call.
func (c *Cookie) Domain() []byte {
	return c.domain
}

// SetDomain sets cookie domain.
func (c *Cookie) SetDomain(domain string) {
	c.domain = append(c.domain[:0], domain...)
}

// SetDomainBytes sets cookie domain.
func (c *Cookie) SetDomainBytes(domain []byte) {
	c.domain = append(c.domain[:0], domain...)
}

// Expire returns cookie expiration time.
//
// CookieExpireUnlimited is returned if cookie doesn't expire
func (c *Cookie) Expire() time.Time {
	expire := c.expire
	if expire.IsZero() {
		expire = CookieExpireUnlimited
	}
	return expire
}

// SetExpire sets cookie expiration time.
//
// Set expiration time to CookieExpireDelete for expiring (deleting)
// the cookie on the client.
//
// By default cookie lifetime is limited by browser session.
func (c *Cookie) SetExpire(expire time.Time) {
	c.expire = expire
}

// Value returns cookie value.
//
// The returned value is valid until the next Cookie modification method call.
func (c *Cookie) Value() []byte {
	return c.value
}

// SetValue sets cookie value.
func (c *Cookie) SetValue(value string) {
	c.value = append(c.value[:0], value...)
}

// SetValueBytes sets cookie value.
func (c *Cookie) SetValueBytes(value []byte) {
	c.value = append(c.value[:0], value...)
}

// Key returns cookie name.
//
// The returned value is valid until the next Cookie modification method call.
func (c *Cookie) Key() []byte {
	return c.key
}

// SetKey sets cookie name.
func (c *Cookie) SetKey(key string) {
	c.key = append(c.key[:0], key...)
}

// SetKeyBytes sets cookie name.
func (c *Cookie) SetKeyBytes(key []byte) {
	c.key = append(c.key[:0], key...)
}

// Reset clears the cookie.
func (c *Cookie) Reset() {
	c.key = c.key[:0]
	c.value = c.value[:0]
	c.expire = zeroTime
	c.domain = c.domain[:0]
	c.path = c.path[:0]
}

// AppendBytes appends cookie representation to dst and returns
// the extended dst.
func (c *Cookie) AppendBytes(dst []byte) []byte {
	if len(c.key) > 0 {
		dst = AppendQuotedArg(dst, c.key)
		dst = append(dst, '=')
	}
	dst = AppendQuotedArg(dst, c.value)

	if !c.expire.IsZero() {
		c.bufKV.value = AppendHTTPDate(c.bufKV.value[:0], c.expire)
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieExpires...)
		dst = append(dst, '=')
		dst = append(dst, c.bufKV.value...)
	}
	if len(c.domain) > 0 {
		dst = appendCookiePart(dst, strCookieDomain, c.domain)
	}
	if len(c.path) > 0 {
		dst = appendCookiePart(dst, strCookiePath, c.path)
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
	c.buf = append(c.buf[:0], src...)
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

	c.key = append(c.key[:0], kv.key...)
	c.value = append(c.value[:0], kv.value...)

	for s.next(kv, false) {
		if len(kv.key) == 0 && len(kv.value) == 0 {
			continue
		}
		switch {
		case bytes.Equal(strCookieExpires, kv.key):
			v := unsafeBytesToStr(kv.value)
			exptime, err := time.ParseInLocation(time.RFC1123, v, time.UTC)
			if err != nil {
				return err
			}
			c.expire = exptime
		case bytes.Equal(strCookieDomain, kv.key):
			c.domain = append(c.domain[:0], kv.value...)
		case bytes.Equal(strCookiePath, kv.key):
			c.path = append(c.path[:0], kv.value...)
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
			dst = AppendQuotedArg(dst, kv.key)
			dst = append(dst, '=')
		}
		dst = AppendQuotedArg(dst, kv.value)
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
