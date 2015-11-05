package fasthttp

import (
	"bytes"
	"errors"
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
}

var zeroTime time.Time

// Clear clears the cookie.
func (c *Cookie) Clear() {
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
		c.bufKV.value = c.Expire.In(gmtLocation).AppendFormat(c.bufKV.value[:0], time.RFC1123)
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

var errNoCookies = errors.New("no cookies found")

// Parse parses Set-Cookie header.
func (c *Cookie) Parse(src []byte) error {
	c.Clear()

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
	return decodeCookieArg(dst[:0], src, true)
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

func parseRequestCookies(cookies []argsKV, src []byte, kv *argsKV) []argsKV {
	var s cookieScanner
	s.b = src
	for s.next(kv, true) {
		if len(kv.key) > 0 || len(kv.value) > 0 {
			cookies = setArg(cookies, kv.key, kv.value)
		}
	}
	return cookies
}

type cookieScanner struct {
	b []byte
}

func (s *cookieScanner) next(kv *argsKV, decode bool) bool {
	if len(s.b) == 0 {
		return false
	}

	var b []byte
	n := bytes.IndexByte(s.b, ';')
	if n < 0 {
		b = s.b
		s.b = s.b[len(s.b):]
	} else {
		b = s.b[:n]
		s.b = s.b[n+1:]
	}

	n = bytes.IndexByte(b, '=')
	if n < 0 {
		kv.key = kv.key[:0]
		kv.value = decodeCookieArg(kv.value[:0], b, decode)
		return true
	}

	kv.key = decodeCookieArg(kv.key[:0], b[:n], decode)
	kv.value = decodeCookieArg(kv.value[:0], b[n+1:], decode)
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
		return append(dst, src...)
	}
	return decodeArg(dst, src, true)
}
