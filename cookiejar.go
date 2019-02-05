package fasthttp

import (
	"bytes"
	"sync"
)

// CookieJar manages cookie storage
type CookieJar struct {
	m           sync.Mutex
	hostCookies map[string][]*Cookie
}

// Get returns the cookies stored from a specific domain.
//
// If there were no cookies related with host returned slice will be nil.
func (cj *CookieJar) Get(host string) (cookies []*Cookie) {
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cookies = cj.hostCookies[host]
	}
	cj.m.Unlock()
	return
}

// Set sets cookies for a specific host.
//
// If the cookie key already exists it will be replaced by the new cookie value.
func (cj *CookieJar) Set(host string, cookies ...*Cookie) {
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		hcs := cj.hostCookies[host]
		for _, cookie := range cookies {
			c := searchCookieByKey(cookie.Key(), hcs)
			if c != nil {
				c.ParseBytes(cookie.Cookie())
			} else {
				// TODO: Make a copy of the cookie?
				hcs = append(hcs, cookie)
			}
		}
		cj.hostCookies[host] = hcs
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
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cj.setKeyValue(host, key, value)
	}
	cj.m.Unlock()
}

func (cj *CookieJar) setKeyValue(host string, key, value []byte) {
	hcs := cj.hostCookies[host]
	c := searchCookieByKey(key, hcs)
	if c == nil {
		c = AcquireCookie()
		c.SetKeyBytes(key)
		cj.hostCookies[host] = append(hcs, c)
	}
	c.SetValueBytes(value)
}

func (cj *CookieJar) dumpTo(host []byte, req *Request) {
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cookies, ok := cj.hostCookies[b2s(host)]
		if ok {
			for _, cookie := range cookies {
				req.Header.SetCookieBytesKV(cookie.Key(), cookie.Value())
			}
		}
	}
	cj.m.Unlock()
}

func (cj *CookieJar) getFrom(host []byte, res *Response) {
	cj.m.Lock()
	{
		hs := b2s(host)
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cookies, ok := cj.hostCookies[hs]
		if !ok {
			// If the key does not exists in the map then
			// we must make a copy for the key.
			hs = string(host)
		}
		res.Header.VisitAllCookie(func(key, value []byte) {
			c := searchCookieByKey(key, cookies)
			if c == nil {
				c = AcquireCookie()
				cookies = append(cookies, c)
			}
			// TODO: `value` key could be different from `c` key?
			c.ParseBytes(value)
		})
		cj.hostCookies[hs] = cookies
	}
	cj.m.Unlock()
}

func (cj *CookieJar) searchCookieByHostKey(host string, key []byte) (cookie *Cookie) {
	cookies, ok := cj.hostCookies[host]
	if ok {
		cookie = searchCookieByKey(key, cookies)
	}
	return
}

func searchCookieByKey(key []byte, cookies []*Cookie) (cookie *Cookie) {
	for _, c := range cookies {
		if bytes.Equal(key, c.Key()) {
			cookie = c
			break
		}
	}
	return
}
