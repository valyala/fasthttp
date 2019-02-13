package fasthttp

import (
	"bytes"
	"testing"
	"time"
)

func checkKeyValue(t *testing.T, cj *CookieJar, cookie *Cookie, uri *URI, n int) {
	cs := cj.Get(uri)
	if len(cs) < n {
		t.Fatalf("Unexpected cookie length: %d. Expected %d", len(cs), n)
	}
	c := cs[n-1]
	if c == nil {
		t.Fatal("got a nil cookie")
	}
	if string(c.Key()) != string(cookie.Key()) {
		t.Fatalf("key mismatch: %s <> %s", c.Key(), cookie.Key())
	}
	if string(c.Value()) != string(cookie.Value()) {
		t.Fatalf("value mismatch: %s <> %s", c.Value(), cookie.Value())
	}
}

func TestCookieJarGet(t *testing.T) {
	url := []byte("http://fasthttp.com/")
	url1 := []byte("http://fasthttp.com/make")
	url11 := []byte("http://fasthttp.com/hola")
	url2 := []byte("http://fasthttp.com/make/fasthttp")
	url3 := []byte("http://fasthttp.com/make/fasthttp/great")
	prefix := []byte("/")
	prefix1 := []byte("/make")
	prefix2 := []byte("/make/fasthttp")
	prefix3 := []byte("/make/fasthttp/great")
	cj := &CookieJar{}

	c1 := &Cookie{}
	c1.SetKey("k")
	c1.SetValue("v")
	c1.SetPath("/make/")

	c2 := &Cookie{}
	c2.SetKey("kk")
	c2.SetValue("vv")
	c2.SetPath("/make/fasthttp")

	c3 := &Cookie{}
	c3.SetKey("kkk")
	c3.SetValue("vvv")
	c3.SetPath("/make/fasthttp/great")

	uri := AcquireURI()
	uri.Parse(nil, url)

	uri1 := AcquireURI()
	uri1.Parse(nil, url1)

	uri11 := AcquireURI()
	uri11.Parse(nil, url11)

	uri2 := AcquireURI()
	uri2.Parse(nil, url2)

	uri3 := AcquireURI()
	uri3.Parse(nil, url3)

	cj.Get(uri1)
	cj.Get(uri11)
	cj.Get(uri2)
	cj.Get(uri3)

	cj.Set(uri1, c1, c2, c3)

	cookies := cj.Get(uri1)
	if len(cookies) != 3 {
		t.Fatalf("Unexpected len. Expected %d. Got %d", 3, len(cookies))
	}
	for _, cookie := range cookies {
		if !bytes.HasPrefix(cookie.Path(), prefix1) {
			t.Fatalf("prefix mismatch: %s<>%s", cookie.Path(), prefix1)
		}
	}

	cookies = cj.Get(uri11)
	if len(cookies) != 0 {
		t.Fatalf("Unexpected len. Expected %d. Got %d", 0, len(cookies))
	}

	cookies = cj.Get(uri2)
	if len(cookies) != 2 {
		t.Fatalf("Unexpected len. Expected %d. Got %d", 2, len(cookies))
	}
	for _, cookie := range cookies {
		if !bytes.HasPrefix(cookie.Path(), prefix2) {
			t.Fatalf("prefix mismatch: %s<>%s", cookie.Path(), prefix2)
		}
	}

	cookies = cj.Get(uri3)
	if len(cookies) != 1 {
		t.Fatalf("Unexpected len. Expected %d. Got %d: %v", 1, len(cookies), cookies)
	}
	for _, cookie := range cookies {
		if !bytes.HasPrefix(cookie.Path(), prefix3) {
			t.Fatalf("prefix mismatch: %s<>%s", cookie.Path(), prefix3)
		}
	}

	cookies = cj.Get(uri)
	if len(cookies) != 3 {
		t.Fatalf("Unexpected len. Expected %d. Got %d", 3, len(cookies))
	}
	for _, cookie := range cookies {
		if !bytes.HasPrefix(cookie.Path(), prefix) {
			t.Fatalf("prefix mismatch: %s<>%s", cookie.Path(), prefix)
		}
	}
}

func TestCookieJarGetExpired(t *testing.T) {
	url1 := []byte("http://fasthttp.com/make/")
	uri1 := AcquireURI()
	uri1.Parse(nil, url1)

	c1 := &Cookie{}
	c1.SetKey("k")
	c1.SetValue("v")
	c1.SetExpire(time.Now().Add(-time.Hour))

	cj := &CookieJar{}
	cj.Set(uri1, c1)

	cookies := cj.Get(uri1)
	if len(cookies) != 0 {
		t.Fatalf("unexpected cookie get result. Expected %d. Got %d", 0, len(cookies))
	}
}

func TestCookieJarSet(t *testing.T) {
	url := []byte("http://fasthttp.com/hello/world")
	cj := &CookieJar{}

	cookie := &Cookie{}
	cookie.SetKey("k")
	cookie.SetValue("v")

	uri := AcquireURI()
	uri.Parse(nil, url)

	cj.Set(uri, cookie)
	checkKeyValue(t, cj, cookie, uri, 1)
}

func TestCookieJarSetRepeatedCookieKeys(t *testing.T) {
	host := "fast.http"
	cj := &CookieJar{}

	uri := AcquireURI()
	uri.SetHost(host)

	cookie := &Cookie{}
	cookie.SetKey("k")
	cookie.SetValue("v")

	cookie2 := &Cookie{}
	cookie2.SetKey("k")
	cookie2.SetValue("v2")

	cookie3 := &Cookie{}
	cookie3.SetKey("key")
	cookie3.SetValue("value")

	cj.Set(uri, cookie, cookie2, cookie3)

	cookies := cj.Get(uri)
	if len(cookies) != 2 {
		t.Fatalf("error getting cookies. Expected %d. Got %d", 2, len(cookies))
	}
	if cookies[0] == cookie2 {
		t.Fatalf("Unexpected cookie (%s)", cookies[0])
	}
	if !bytes.Equal(cookies[0].Value(), cookie2.Value()) {
		t.Fatalf("Unexpected cookie value. Expected %s. Got %s", cookies[0].Value(), cookie2.Value())
	}
}

func TestCookieJarSetKeyValue(t *testing.T) {
	host := "fast.http"
	cj := &CookieJar{}

	uri := AcquireURI()
	uri.SetHost(host)

	cj.SetKeyValue(host, "k", "v")
	cj.SetKeyValue(host, "key", "value")
	cj.SetKeyValue(host, "k", "vv")
	cj.SetKeyValue(host, "key", "value2")

	cookies := cj.Get(uri)
	if len(cookies) != 2 {
		t.Fatalf("error getting cookies. Expected %d. Got %d: %v", 2, len(cookies), cookies)
	}
}

func TestCookieJarGetFromResponse(t *testing.T) {
	res := AcquireResponse()
	host := []byte("fast.http")
	uri := AcquireURI()
	uri.SetHostBytes(host)

	c := &Cookie{}
	c.SetKey("key")
	c.SetValue("val")

	c2 := &Cookie{}
	c2.SetKey("k")
	c2.SetValue("v")

	c3 := &Cookie{}
	c3.SetKey("kk")
	c3.SetValue("vv")

	res.Header.SetStatusCode(200)
	res.Header.SetCookie(c)
	res.Header.SetCookie(c2)
	res.Header.SetCookie(c3)

	cj := &CookieJar{}
	cj.getFrom(host, res)

	cookies := cj.Get(uri)
	if len(cookies) != 3 {
		t.Fatalf("error cookies length. Expected %d. Got %d", 3, len(cookies))
	}
}
