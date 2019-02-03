package fasthttp

import (
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
		hs := string(host)
		if cj.hostCookies == nil {
			cj.hostCookies = make(map[string][]*Cookie)
		}
		cookies := cj.hostCookies[hs]
		res.Header.VisitAllCookie(func(key, value []byte) {
			cookie := AcquireCookie()
			cookie.SetKeyBytes(key)
			cookie.ParseBytes(value)
			cookies = append(cookies, cookie)
		})
		cj.hostCookies[hs] = cookies
	}
	cj.m.Unlock()
}
