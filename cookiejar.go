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

// Get returns the cookies stored from a specific domain.
//
// If there were no cookies related with host returned slice will be nil.
//
// CookieJar keeps a copy of the cookies, so the returned cookies can be released safely.
func (cj *CookieJar) Get(uri *URI) (cookies []*Cookie) {
	if uri != nil {
		cookies = cj.get(uri.Host(), uri.Path())
	}
	return
}

func (cj *CookieJar) get(host, path []byte) (rcs []*Cookie) {
	if cj.hostCookies == nil {
		return
	}

	var (
		err     error
		cookies []*Cookie
		hostStr = b2s(host)
	)
	// port must not be included.
	hostStr, _, err = net.SplitHostPort(hostStr)
	if err != nil {
		hostStr = b2s(host)
	}
	// get cookies deleting expired ones
	cookies = cj.getCookies(hostStr)

	rcs = make([]*Cookie, 0, len(cookies))
	for i := 0; i < len(cookies); i++ {
		cookie := cookies[i]
		if len(path) > 1 && len(cookie.path) > 1 && !bytes.HasPrefix(cookie.Path(), path) {
			continue
		}
		rcs = append(rcs, cookie)
	}

	return
}

// getCookies returns a cookie slice releasing expired cookies
func (cj *CookieJar) getCookies(hostStr string) (cookies []*Cookie) {
	cj.m.Lock()
	defer cj.m.Unlock()

	cookies = cj.hostCookies[hostStr]
	var (
		t = time.Now()
		n = len(cookies)
	)
	for i := 0; i < len(cookies); i++ {
		c := cookies[i]
		if !c.Expire().Equal(CookieExpireUnlimited) && c.Expire().Before(t) { // cookie expired
			cookies = append(cookies[:i], cookies[i+1:]...)
			ReleaseCookie(c)
			i--
		}
	}
	// has any cookie been deleted?
	if n > len(cookies) {
		cj.hostCookies[hostStr] = cookies
	}
	return
}

// Set sets cookies for a specific host.
//
// The host is get from uri.Host().
//
// If the cookie key already exists it will be replaced by the new cookie value.
//
// CookieJar keeps a copy of the cookies, so the parsed cookies can be released safely.
func (cj *CookieJar) Set(uri *URI, cookies ...*Cookie) {
	if uri != nil {
		cj.set(uri.Host(), cookies...)
	}
}

// SetByHost sets cookies for a specific host.
//
// If the cookie key already exists it will be replaced by the new cookie value.
//
// CookieJar keeps a copy of the cookies, so the parsed cookies can be released safely.
func (cj *CookieJar) SetByHost(host []byte, cookies ...*Cookie) {
	cj.set(host, cookies...)
}

func (cj *CookieJar) set(host []byte, cookies ...*Cookie) {
	hostStr := b2s(host)

	cj.m.Lock()
	defer cj.m.Unlock()

	if cj.hostCookies == nil {
		cj.hostCookies = make(map[string][]*Cookie)
	}
	hcs, ok := cj.hostCookies[hostStr]
	if !ok {
		// If the key does not exists in the map then
		// we must make a copy for the key.
		hostStr = string(host)
	}
	for _, cookie := range cookies {
		c := searchCookieByKeyAndPath(cookie.Key(), cookie.Path(), hcs)
		if c == nil {
			c = AcquireCookie()
			hcs = append(hcs, c)
		}
		c.CopyTo(cookie)
	}
	cj.hostCookies[hostStr] = hcs
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
	hostStr := b2s(host)

	cj.m.Lock()
	defer cj.m.Unlock()

	if cj.hostCookies == nil {
		cj.hostCookies = make(map[string][]*Cookie)
	}
	cookies, ok := cj.hostCookies[hostStr]
	if !ok {
		// If the key does not exists in the map then
		// we must make a copy for the key.
		hostStr = string(host)
	}
	t := time.Now()
	res.Header.VisitAllCookie(func(key, value []byte) {
		created := false
		c := searchCookieByKeyAndPath(key, path, cookies)
		if c == nil {
			c, created = AcquireCookie(), true
		}
		c.ParseBytes(value)
		if c.Expire().Equal(CookieExpireUnlimited) || c.Expire().After(t) {
			cookies = append(cookies, c)
		} else if created {
			ReleaseCookie(c)
		}
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
