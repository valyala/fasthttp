package fasthttp

import (
	"sync"
)

// CookieJar manages cookie storage
type CookieJar struct {
	m           sync.RWMutex
	hostCookies map[string][]*Cookie
}

// Get returns the cookies stored from a specific domain.
//
// If there were no cookies related with host returned slice will be nil.
func (cj *CookieJar) Get(host string) (cookies []*Cookie) {
	cj.m.RLock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cookies = cj.hostCookies[host]
	}
	cj.m.RUnlock()
	return
}

// Set sets cookies for a specific host.
func (cj *CookieJar) Set(host string, cookies ...*Cookie) {
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		hc := cj.hostCookies[host]
		hc = append(hc, cookies...)
		cj.hostCookies[host] = hc
	}
	cj.m.Unlock()
}

func (cj *CookieJar) dumpTo(host string, req *Request) {
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cookies, ok := cj.hostCookies[host]
		if ok {
			for _, cookie := range cookies {
				req.Header.SetCookieBytesKV(cookie.Key(), cookie.Value())
			}
		}
	}
	cj.m.Unlock()
}

func (cj *CookieJar) getFrom(host string, res *Response) {
	cj.m.Lock()
	{
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cookies, ok := cj.hostCookies[host]
		if !ok {
			cookies = make([]*Cookie, 0)
		}
		res.Header.VisitAllCookie(func(key, value []byte) {
			cookie := &Cookie{}
			cookie.SetKeyBytes(key)
			if !res.Header.Cookie(cookie) {
				// TODO: error?
				cookie.SetValueBytes(value)
			}
			cookies = append(cookies, cookie)
		})
		cj.hostCookies[host] = cookies
	}
	cj.m.Unlock()
}
