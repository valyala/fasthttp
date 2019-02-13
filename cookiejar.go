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
func (cj *CookieJar) Get(uri *URI) (rcs []*Cookie) {
	if uri != nil {
		var err error
		host := uri.Host()
		path := uri.Path()
		hostStr := b2s(host)
		// port must not be included.
		if hasPort(host) {
			hostStr, _, err = net.SplitHostPort(hostStr)
		}
		if err == nil {
			cj.m.Lock()
			{
				if cj.hostCookies == nil {
					cj.hostCookies = make(map[string][]*Cookie)
				} else {
					// get cookies deleting expired ones
					cookies := cj.getCookies(hostStr)
					if len(cookies) > 0 {
						rcs = copyCookies(cookies) // make a copy
						for i := 0; i < len(rcs); i++ {
							cookie := rcs[i]
							if len(path) > 1 && len(cookie.path) > 1 { // path > "/"
								// In this case calculating the len will be enough.
								// if we have path = '/some/path' and cookie.Path() = '/some'
								switch {
								case len(path) > len(cookie.path): // path differs
									fallthrough
								case !bytes.HasPrefix(cookie.path, path):
									rcs = append(rcs[:i], rcs[i+1:]...)
									ReleaseCookie(cookie)
									i--
								}
							}
						}
					}
				}
			}
			cj.m.Unlock()
		}
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
	if n > len(cookies) {
		cj.hostCookies[hostStr] = cookies
	}
	return cookies
}

// Set sets cookies for a specific host.
//
// If the cookie key already exists it will be replaced by the new cookie value.
func (cj *CookieJar) Set(uri *URI, cookies ...*Cookie) {
	if uri != nil {
		cj.m.Lock()
		{
			host := uri.Host()
			hostStr := b2s(host)
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
				c := searchCookieByKey(cookie.key, hcs)
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
