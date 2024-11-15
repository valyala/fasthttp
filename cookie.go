package fasthttp

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"
)

var zeroTime time.Time

var (
	// CookieExpireDelete may be set on Cookie.Expire for expiring the given cookie.
	CookieExpireDelete = time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)

	// CookieExpireUnlimited indicates that the cookie doesn't expire.
	CookieExpireUnlimited = zeroTime
)

// CookieSameSite is an enum for the mode in which the SameSite flag should be set for the given cookie.
// See https://tools.ietf.org/html/draft-ietf-httpbis-cookie-same-site-00 for details.
type CookieSameSite int

const (
	// CookieSameSiteDisabled removes the SameSite flag.
	CookieSameSiteDisabled CookieSameSite = iota
	// CookieSameSiteDefaultMode sets the SameSite flag.
	CookieSameSiteDefaultMode
	// CookieSameSiteLaxMode sets the SameSite flag with the "Lax" parameter.
	CookieSameSiteLaxMode
	// CookieSameSiteStrictMode sets the SameSite flag with the "Strict" parameter.
	CookieSameSiteStrictMode
	// CookieSameSiteNoneMode sets the SameSite flag with the "None" parameter.
	// See https://tools.ietf.org/html/draft-west-cookie-incrementalism-00
	CookieSameSiteNoneMode // third-party cookies are phasing out, use Partitioned cookies instead
)

// AcquireCookie returns an empty Cookie object from the pool.
//
// The returned object may be returned back to the pool with ReleaseCookie.
// This allows reducing GC load.
func AcquireCookie() *Cookie {
	return cookiePool.Get().(*Cookie)
}

// ReleaseCookie returns the Cookie object acquired with AcquireCookie back
// to the pool.
//
// Do not access released Cookie object, otherwise data races may occur.
func ReleaseCookie(c *Cookie) {
	c.Reset()
	cookiePool.Put(c)
}

var cookiePool = &sync.Pool{
	New: func() any {
		return &Cookie{}
	},
}

// Cookie represents HTTP response cookie.
//
// Do not copy Cookie objects. Create new object and use CopyTo instead.
//
// Cookie instance MUST NOT be used from concurrently running goroutines.
type Cookie struct {
	noCopy noCopy

	expire time.Time

	key    []byte
	value  []byte
	domain []byte
	path   []byte

	bufK []byte
	bufV []byte

	// maxAge=0 means no 'max-age' attribute specified.
	// maxAge<0 means delete cookie now, equivalently 'max-age=0'
	// maxAge>0 means 'max-age' attribute present and given in seconds
	maxAge int

	sameSite    CookieSameSite
	httpOnly    bool
	secure      bool
	partitioned bool
}

// CopyTo copies src cookie to c.
func (c *Cookie) CopyTo(src *Cookie) {
	c.Reset()
	c.key = append(c.key, src.key...)
	c.value = append(c.value, src.value...)
	c.expire = src.expire
	c.maxAge = src.maxAge
	c.domain = append(c.domain, src.domain...)
	c.path = append(c.path, src.path...)
	c.httpOnly = src.httpOnly
	c.secure = src.secure
	c.sameSite = src.sameSite
	c.partitioned = src.partitioned
}

// HTTPOnly returns true if the cookie is http only.
func (c *Cookie) HTTPOnly() bool {
	return c.httpOnly
}

// SetHTTPOnly sets cookie's httpOnly flag to the given value.
func (c *Cookie) SetHTTPOnly(httpOnly bool) {
	c.httpOnly = httpOnly
}

// Secure returns true if the cookie is secure.
func (c *Cookie) Secure() bool {
	return c.secure
}

// SetSecure sets cookie's secure flag to the given value.
func (c *Cookie) SetSecure(secure bool) {
	c.secure = secure
}

// SameSite returns the SameSite mode.
func (c *Cookie) SameSite() CookieSameSite {
	return c.sameSite
}

// SetSameSite sets the cookie's SameSite flag to the given value.
// Set value CookieSameSiteNoneMode will set Secure to true also to avoid browser rejection.
func (c *Cookie) SetSameSite(mode CookieSameSite) {
	c.sameSite = mode
	if mode == CookieSameSiteNoneMode {
		c.SetSecure(true)
	}
}

// Partitioned returns true if the cookie is partitioned.
func (c *Cookie) Partitioned() bool {
	return c.partitioned
}

// SetPartitioned sets the cookie's Partitioned flag to the given value.
// Set value Partitioned to true will set Secure to true and Path to / also to avoid browser rejection.
func (c *Cookie) SetPartitioned(partitioned bool) {
	c.partitioned = partitioned
	if partitioned {
		c.SetSecure(true)
		c.SetPath("/")
	}
}

// Path returns cookie path.
func (c *Cookie) Path() []byte {
	return c.path
}

// SetPath sets cookie path.
func (c *Cookie) SetPath(path string) {
	c.bufK = append(c.bufK[:0], path...)
	c.path = normalizePath(c.path, c.bufK)
}

// SetPathBytes sets cookie path.
func (c *Cookie) SetPathBytes(path []byte) {
	c.bufK = append(c.bufK[:0], path...)
	c.path = normalizePath(c.path, c.bufK)
}

// Domain returns cookie domain.
//
// The returned value is valid until the Cookie reused or released (ReleaseCookie).
// Do not store references to the returned value. Make copies instead.
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

// MaxAge returns the seconds until the cookie is meant to expire or 0
// if no max age.
func (c *Cookie) MaxAge() int {
	return c.maxAge
}

// SetMaxAge sets cookie expiration time based on seconds. This takes precedence
// over any absolute expiry set on the cookie.
//
// 'max-age' is set when the maxAge is non-zero. That is, if maxAge = 0,
// the 'max-age' is unset. If maxAge < 0, it indicates that the cookie should
// be deleted immediately, equivalent to 'max-age=0'. This behavior is
// consistent with the Go standard library's net/http package.
func (c *Cookie) SetMaxAge(seconds int) {
	c.maxAge = seconds
}

// Expire returns cookie expiration time.
//
// CookieExpireUnlimited is returned if cookie doesn't expire.
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
// The returned value is valid until the Cookie reused or released (ReleaseCookie).
// Do not store references to the returned value. Make copies instead.
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
// The returned value is valid until the Cookie reused or released (ReleaseCookie).
// Do not store references to the returned value. Make copies instead.
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
	c.maxAge = 0
	c.domain = c.domain[:0]
	c.path = c.path[:0]
	c.httpOnly = false
	c.secure = false
	c.sameSite = CookieSameSiteDisabled
	c.partitioned = false
}

// AppendBytes appends cookie representation to dst and returns
// the extended dst.
func (c *Cookie) AppendBytes(dst []byte) []byte {
	if len(c.key) > 0 {
		dst = append(dst, c.key...)
		dst = append(dst, '=')
	}
	dst = append(dst, c.value...)

	if c.maxAge != 0 {
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieMaxAge...)
		dst = append(dst, '=')
		if c.maxAge < 0 {
			// See https://github.com/valyala/fasthttp/issues/1900
			dst = AppendUint(dst, 0)
		} else {
			dst = AppendUint(dst, c.maxAge)
		}
	} else if !c.expire.IsZero() {
		c.bufV = AppendHTTPDate(c.bufV[:0], c.expire)
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieExpires...)
		dst = append(dst, '=')
		dst = append(dst, c.bufV...)
	}
	if len(c.domain) > 0 {
		dst = appendCookiePart(dst, strCookieDomain, c.domain)
	}
	if len(c.path) > 0 {
		dst = appendCookiePart(dst, strCookiePath, c.path)
	}
	if c.httpOnly {
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieHTTPOnly...)
	}
	if c.secure {
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieSecure...)
	}
	switch c.sameSite {
	case CookieSameSiteDefaultMode:
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieSameSite...)
	case CookieSameSiteLaxMode:
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieSameSite...)
		dst = append(dst, '=')
		dst = append(dst, strCookieSameSiteLax...)
	case CookieSameSiteStrictMode:
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieSameSite...)
		dst = append(dst, '=')
		dst = append(dst, strCookieSameSiteStrict...)
	case CookieSameSiteNoneMode:
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookieSameSite...)
		dst = append(dst, '=')
		dst = append(dst, strCookieSameSiteNone...)
	}
	if c.partitioned {
		dst = append(dst, ';', ' ')
		dst = append(dst, strCookiePartitioned...)
	}
	return dst
}

// Cookie returns cookie representation.
//
// The returned value is valid until the Cookie reused or released (ReleaseCookie).
// Do not store references to the returned value. Make copies instead.
func (c *Cookie) Cookie() []byte {
	c.bufK = c.AppendBytes(c.bufK[:0])
	return c.bufK
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
	c.bufK = append(c.bufK[:0], src...)
	return c.ParseBytes(c.bufK)
}

// ParseBytes parses Set-Cookie header.
func (c *Cookie) ParseBytes(src []byte) error {
	c.Reset()

	var s cookieScanner
	s.b = src

	if !s.next(&c.bufK, &c.bufV) {
		return errNoCookies
	}

	c.key = append(c.key, c.bufK...)
	c.value = append(c.value, c.bufV...)

	for s.next(&c.bufK, &c.bufV) {
		if len(c.bufK) != 0 {
			// Case insensitive switch on first char
			switch c.bufK[0] | 0x20 {
			case 'm':
				if caseInsensitiveCompare(strCookieMaxAge, c.bufK) {
					maxAge, err := ParseUint(c.bufV)
					if err != nil {
						return err
					}
					c.maxAge = maxAge
				}

			case 'e': // "expires"
				if caseInsensitiveCompare(strCookieExpires, c.bufK) {
					v := b2s(c.bufV)
					// Try the same two formats as net/http
					// See: https://github.com/golang/go/blob/00379be17e63a5b75b3237819392d2dc3b313a27/src/net/http/cookie.go#L133-L135
					exptime, err := time.ParseInLocation(time.RFC1123, v, time.UTC)
					if err != nil {
						exptime, err = time.Parse("Mon, 02-Jan-2006 15:04:05 MST", v)
						if err != nil {
							return err
						}
					}
					c.expire = exptime
				}

			case 'd': // "domain"
				if caseInsensitiveCompare(strCookieDomain, c.bufK) {
					c.domain = append(c.domain, c.bufV...)
				}

			case 'p': // "path"
				if caseInsensitiveCompare(strCookiePath, c.bufK) {
					c.path = append(c.path, c.bufV...)
				}

			case 's': // "samesite"
				if caseInsensitiveCompare(strCookieSameSite, c.bufK) {
					if len(c.bufV) > 0 {
						// Case insensitive switch on first char
						switch c.bufV[0] | 0x20 {
						case 'l': // "lax"
							if caseInsensitiveCompare(strCookieSameSiteLax, c.bufV) {
								c.sameSite = CookieSameSiteLaxMode
							}
						case 's': // "strict"
							if caseInsensitiveCompare(strCookieSameSiteStrict, c.bufV) {
								c.sameSite = CookieSameSiteStrictMode
							}
						case 'n': // "none"
							if caseInsensitiveCompare(strCookieSameSiteNone, c.bufV) {
								c.sameSite = CookieSameSiteNoneMode
							}
						}
					}
				}
			}
		} else if len(c.bufV) != 0 {
			// Case insensitive switch on first char
			switch c.bufV[0] | 0x20 {
			case 'h': // "httponly"
				if caseInsensitiveCompare(strCookieHTTPOnly, c.bufV) {
					c.httpOnly = true
				}

			case 's': // "secure"
				if caseInsensitiveCompare(strCookieSecure, c.bufV) {
					c.secure = true
				} else if caseInsensitiveCompare(strCookieSameSite, c.bufV) {
					c.sameSite = CookieSameSiteDefaultMode
				}
			case 'p': // "partitioned"
				if caseInsensitiveCompare(strCookiePartitioned, c.bufV) {
					c.partitioned = true
				}
			}
		} // else empty or no match
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
	return decodeCookieArg(dst, src, false)
}

func appendRequestCookieBytes(dst []byte, cookies []argsKV) []byte {
	for i, n := 0, len(cookies); i < n; i++ {
		kv := &cookies[i]
		if len(kv.key) > 0 {
			dst = append(dst, kv.key...)
			dst = append(dst, '=')
		}
		dst = append(dst, kv.value...)
		if i+1 < n {
			dst = append(dst, ';', ' ')
		}
	}
	return dst
}

// For Response we can not use the above function as response cookies
// already contain the key= in the value.
func appendResponseCookieBytes(dst []byte, cookies []argsKV) []byte {
	for i, n := 0, len(cookies); i < n; i++ {
		kv := &cookies[i]
		dst = append(dst, kv.value...)
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
	for s.next(&kv.key, &kv.value) {
		if len(kv.key) > 0 || len(kv.value) > 0 {
			cookies, kv = allocArg(cookies)
		}
	}
	return releaseArg(cookies)
}

type cookieScanner struct {
	b []byte
}

func (s *cookieScanner) next(key, val *[]byte) bool {
	b := s.b
	if len(b) == 0 {
		return false
	}

	isKey := true
	k := 0
	for i, c := range b {
		switch c {
		case '=':
			if isKey {
				isKey = false
				*key = decodeCookieArg(*key, b[:i], false)
				k = i + 1
			}
		case ';':
			if isKey {
				*key = (*key)[:0]
			}
			*val = decodeCookieArg(*val, b[k:i], true)
			s.b = b[i+1:]
			return true
		}
	}

	if isKey {
		*key = (*key)[:0]
	}
	*val = decodeCookieArg(*val, b[k:], true)
	s.b = b[len(b):]
	return true
}

func decodeCookieArg(dst, src []byte, skipQuotes bool) []byte {
	for len(src) > 0 && src[0] == ' ' {
		src = src[1:]
	}
	for len(src) > 0 && src[len(src)-1] == ' ' {
		src = src[:len(src)-1]
	}
	if skipQuotes {
		if len(src) > 1 && src[0] == '"' && src[len(src)-1] == '"' {
			src = src[1 : len(src)-1]
		}
	}
	return append(dst[:0], src...)
}

// caseInsensitiveCompare does a case insensitive equality comparison of
// two []byte. Assumes only letters need to be matched.
func caseInsensitiveCompare(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i]|0x20 != b[i]|0x20 {
			return false
		}
	}
	return true
}
