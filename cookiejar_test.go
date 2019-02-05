package fasthttp

import (
	"bytes"
	"testing"
)

func prepareJar() *CookieJar {
	cj := &CookieJar{}
	cj.hostCookies = make(map[string][]*Cookie)
	return cj
}

func checkKeyValue(t *testing.T, cj *CookieJar, cookie *Cookie, host string, n int) {
	cs := cj.Get(host)
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
	host := "fast.http"
	cj := prepareJar()

	cookie := &Cookie{}
	cookie.SetKey("k")
	cookie.SetValue("v")

	cj.hostCookies[host] = append(cj.hostCookies[host], cookie)
	checkKeyValue(t, cj, cookie, host, 1)
}

func TestCookieJarSet(t *testing.T) {
	host := "fast.http"
	cj := &CookieJar{}

	cookie := &Cookie{}
	cookie.SetKey("k")
	cookie.SetValue("v")

	cj.Set(host, cookie)
	checkKeyValue(t, cj, cookie, host, 1)
}

func TestCookieJarSetRepeatedCookieKeys(t *testing.T) {
	host := "fast.http"
	cj := &CookieJar{}

	cookie := &Cookie{}
	cookie.SetKey("k")
	cookie.SetValue("v")

	cookie2 := &Cookie{}
	cookie2.SetKey("k")
	cookie2.SetValue("v2")

	cookie3 := &Cookie{}
	cookie3.SetKey("key")
	cookie3.SetValue("value")

	cj.Set(host, cookie, cookie2, cookie3)

	cookies := cj.Get(host)
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

	cj.SetKeyValue(host, "k", "v")
	cj.SetKeyValue(host, "key", "value")
	cj.SetKeyValue(host, "k", "vv")
	cj.SetKeyValue(host, "key", "value2")

	cookies := cj.Get(host)
	if len(cookies) != 2 {
		t.Fatalf("error getting cookies. Expected %d. Got %d: %v", 2, len(cookies), cookies)
	}
}
