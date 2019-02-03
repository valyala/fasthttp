package fasthttp

import "testing"

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

func TestCookieJar_Get(t *testing.T) {
	host := "fast.http"
	cj := prepareJar()

	cookie := &Cookie{}
	cookie.SetKey("k")
	cookie.SetValue("v")

	cj.hostCookies[host] = append(cj.hostCookies[host], cookie)
	checkKeyValue(t, cj, cookie, host, 1)
}

func TestCookieJar_Set(t *testing.T) {
	host := "fast.http"
	cj := &CookieJar{}

	cookie := &Cookie{}
	cookie.SetKey("k")
	cookie.SetValue("v")

	cj.Set(host, cookie)
	checkKeyValue(t, cj, cookie, host, 1)
}
