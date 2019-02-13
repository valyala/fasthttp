package fasthttp

import (
	"bytes"
	"net"
	"sync"
	"time"
)

// CookieJar manages cookie storage
type CookieJar struct {
	m           sync.Mutex
	hostCookies map[string][]*Cookie
}

func (cj *CookieJar) init() {
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
	}
	cj.m.Unlock()
}

func hasPort(host []byte) bool {
	return bytes.IndexByte(host, ':') > 0
}

func copyCookies(cookies []*Cookie) (cs []*Cookie) {
	// TODO: Try to delete the allocations
	cs = make([]*Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		c := AcquireCookie()
		c.CopyTo(cookie)
		cs = append(cs, c)
	}
	return
}

// Get returns the cookies stored from a specific domain.
//
// If there were no cookies related with host returned slice will be nil.
func (cj *CookieJar) Get(uri *URI) (cookies []*Cookie) {
	if uri != nil {
		cookies = cj.get(uri.Host(), uri.Path())
	}
	return
}

func (cj *CookieJar) get(host, path []byte) (rcs []*Cookie) {
	cj.init()

	var (
		err     error
		hostStr = b2s(host)
	)
	// port must not be included.
	if hasPort(host) {
		hostStr, _, err = net.SplitHostPort(hostStr)
	}
	if err == nil {
		cj.m.Lock()
		{
			// get cookies deleting expired ones
			cookies := cj.getCookies(hostStr)
			if len(cookies) > 0 {
				rcs = copyCookies(cookies) // make a copy
				for i := 0; i < len(rcs); i++ {
					cookie := rcs[i]
					if len(path) > 1 && len(cookie.path) > 1 && !bytes.HasPrefix(cookie.path, path) {
						rcs = append(rcs[:i], rcs[i+1:]...)
						ReleaseCookie(cookie)
						i--
					}
				}
			}
		}
		cj.m.Unlock()
	}
	return
}

// getCookies returns a cookie slice releasing expired cookies
func (cj *CookieJar) getCookies(hostStr string) []*Cookie {
	var (
		cookies = cj.hostCookies[hostStr]
		t       = time.Now()
		n       = len(cookies)
	)
	for i := 0; i < len(cookies); i++ {
		c := cookies[i]
		if !c.expire.IsZero() && t.Sub(c.expire) > 0 { // expired
			cookies = append(cookies[:i], cookies[i+1:]...)
			ReleaseCookie(c)
			i--
		}
	}
	// has any cookie been deleted?
	if n > len(cookies) {
		cj.hostCookies[hostStr] = cookies
	}
	return cookies
}

// Set sets cookies for a specific host.
//
// The host is get from uri.Host().
//
// If the cookie key already exists it will be replaced by the new cookie value.
func (cj *CookieJar) Set(uri *URI, cookies ...*Cookie) {
	if uri != nil {
		cj.set(uri.Host(), cookies...)
	}
}

// SetByHost sets cookies for a specific host.
//
// If the cookie key already exists it will be replaced by the new cookie value.
func (cj *CookieJar) SetByHost(host []byte, cookies ...*Cookie) {
	cj.set(host, cookies...)
}

func (cj *CookieJar) set(host []byte, cookies ...*Cookie) {
	cj.init()

	cj.m.Lock()
	{
		hostStr := b2s(host)
		hcs, ok := cj.hostCookies[hostStr]
		if !ok {
			// If the key does not exists in the map then
			// we must make a copy for the key.
			hostStr = string(host)
		}
		for _, cookie := range cookies {
			c := searchCookieByKeyAndPath(cookie.Key(), cookie.Path(), hcs)
			if c != nil {
				c.ParseBytes(cookie.Cookie())
			} else {
				c = AcquireCookie()
				c.CopyTo(cookie)
				hcs = append(hcs, c)
			}
		}
		cj.hostCookies[hostStr] = hcs
	}
	cj.m.Unlock()
}

// SetKeyValue sets a cookie by key and value for a specific host.
//
// This function prevents extra allocations by making repeated cookies
// not being duplicated.
func (cj *CookieJar) SetKeyValue(host, key, value string) {
	cj.SetKeyValueBytes(host, s2b(key), s2b(value))
}

// SetKeyValueBytes sets a cookie by key and value for a specific host.
//
// This function prevents extra allocations by making repeated cookies
// not being duplicated.
func (cj *CookieJar) SetKeyValueBytes(host string, key, value []byte) {
	cj.setKeyValue(host, key, value)
}

func (cj *CookieJar) setKeyValue(host string, key, value []byte) {
	c := AcquireCookie()
	c.SetKeyBytes(key)
	c.SetValueBytes(value)
	cj.set(s2b(host), c)
}

func (cj *CookieJar) dumpTo(req *Request) {
	uri := req.URI()
	cookies := cj.get(uri.Host(), uri.Path())
	for _, cookie := range cookies {
		req.Header.SetCookieBytesKV(cookie.Key(), cookie.Value())
	}
}

func (cj *CookieJar) getFrom(host, path []byte, res *Response) {
	cj.init()

	hostStr := b2s(host)
	cookies, ok := cj.hostCookies[hostStr]
	if !ok {
		// If the key does not exists in the map then
		// we must make a copy for the key.
		hostStr = string(host)
	}
	res.Header.VisitAllCookie(func(key, value []byte) {
		c := searchCookieByKeyAndPath(key, path, cookies)
		if c == nil {
			c = AcquireCookie()
			cookies = append(cookies, c)
		}
		c.ParseBytes(value)
	})
	cj.hostCookies[hostStr] = cookies
}

func searchCookieByKeyAndPath(key, path []byte, cookies []*Cookie) (cookie *Cookie) {
	for _, c := range cookies {
		if bytes.Equal(key, c.Key()) {
			if len(path) <= 1 || bytes.HasPrefix(c.Path(), path) {
				cookie = c
				break
			}
		}
	}
	return
}
