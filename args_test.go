package fasthttp

import (
	"fmt"
	"strings"
	"testing"
)

func TestArgsStringCompose(t *testing.T) {
	var a Args
	a.Set("foo", "bar")
	a.Set("aa", "bbb")
	a.Set("привет", "мир")
	a.Set("", "xxxx")
	a.Set("cvx", "")

	expectedS := "foo=bar&aa=bbb&%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82=%D0%BC%D0%B8%D1%80&=xxxx&cvx"
	s := a.String()
	if s != expectedS {
		t.Fatalf("Unexpected string %q. Exected %q", s, expectedS)
	}
}

func TestArgsString(t *testing.T) {
	var a Args

	testArgsString(t, &a, "")
	testArgsString(t, &a, "foobar")
	testArgsString(t, &a, "foo=bar")
	testArgsString(t, &a, "foo=bar&baz=sss")
	testArgsString(t, &a, "")
	testArgsString(t, &a, "f%20o=x.x/x%D0%BF%D1%80%D0%B8%D0%B2%D0%B5aaa&sdf=ss")
	testArgsString(t, &a, "=asdfsdf")
}

func testArgsString(t *testing.T, a *Args, s string) {
	a.Parse(s)
	s1 := a.String()
	if s != s1 {
		t.Fatalf("Unexpected args %q. Expected %q", s1, s)
	}
}

func TestArgsSetGetDel(t *testing.T) {
	var a Args

	if a.Get("foo") != "" {
		t.Fatalf("Unexpected value: %q", a.Get("foo"))
	}
	if a.Get("") != "" {
		t.Fatalf("Unexpected value: %q", a.Get(""))
	}
	a.Del("xxx")

	for j := 0; j < 3; j++ {
		for i := 0; i < 10; i++ {
			k := fmt.Sprintf("foo%d", i)
			v := fmt.Sprintf("bar_%d", i)
			a.Set(k, v)
			if a.Get(k) != v {
				t.Fatalf("Unexpected value: %q. Expected %q", a.Get(k), v)
			}
		}
	}
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("foo%d", i)
		v := fmt.Sprintf("bar_%d", i)
		if a.Get(k) != v {
			t.Fatalf("Unexpected value: %q. Expected %q", a.Get(k), v)
		}
		a.Del(k)
		if a.Get(k) != "" {
			t.Fatalf("Unexpected value: %q. Expected %q", a.Get(k), "")
		}
	}

	a.Parse("aaa=xxx&bb=aa")
	if a.Get("foo0") != "" {
		t.Fatalf("Unepxected value %q", a.Get("foo0"))
	}
	if a.Get("aaa") != "xxx" {
		t.Fatalf("Unexpected value %q. Expected %q", a.Get("aaa"), "xxx")
	}
	if a.Get("bb") != "aa" {
		t.Fatalf("Unexpected value %q. Expected %q", a.Get("bb"), "aa")
	}

	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("xx%d", i)
		v := fmt.Sprintf("yy%d", i)
		a.Set(k, v)
		if a.Get(k) != v {
			t.Fatalf("Unexpected value: %q. Expected %q", a.Get(k), v)
		}
	}
	for i := 5; i < 10; i++ {
		k := fmt.Sprintf("xx%d", i)
		v := fmt.Sprintf("yy%d", i)
		if a.Get(k) != v {
			t.Fatalf("Unexpected value: %q. Expected %q", a.Get(k), v)
		}
		a.Del(k)
		if a.Get(k) != "" {
			t.Fatalf("Unexpected value: %q. Expected %q", a.Get(k), "")
		}
	}
}

func TestArgsParse(t *testing.T) {
	var a Args

	// empty args
	testArgsParse(t, &a, "", 0, "foo=", "bar=", "=")

	// arg without value
	testArgsParse(t, &a, "foo1", 1, "foo=", "bar=", "=")

	// arg without value, but with equal sign
	testArgsParse(t, &a, "foo2=", 1, "foo=", "bar=", "=")

	// arg with value
	testArgsParse(t, &a, "foo3=bar1", 1, "foo3=bar1", "bar=", "=")

	// empty key
	testArgsParse(t, &a, "=bar2", 1, "foo=", "=bar2", "bar2=")

	// missing kv
	testArgsParse(t, &a, "&&&&", 0, "foo=", "bar=", "=")

	// multiple values with the same key
	testArgsParse(t, &a, "x=1&x=2&x=3", 3, "x=1")

	// multiple args
	testArgsParse(t, &a, "&&&qw=er&tyx=124&&&zxc_ss=2234&&", 3, "qw=er", "tyx=124", "zxc_ss=2234")

	// multiple args without values
	testArgsParse(t, &a, "&&a&&b&&bar&baz", 4, "a=", "b=", "bar=", "baz=")

	// values with '='
	testArgsParse(t, &a, "zz=1&k=v=v=a=a=s", 2, "k=v=v=a=a=s", "zz=1")

	// mixed '=' and '&'
	testArgsParse(t, &a, "sss&z=dsf=&df", 3, "sss=", "z=dsf=", "df=")

	// encoded args
	testArgsParse(t, &a, "f+o%20o=%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82+test", 1, "f o o=привет test")

	// invalid percent encoding
	testArgsParse(t, &a, "f%=x&qw%z=d%0k%20p&%%20=%%%20x", 3, "f%=x", "qw%z=d%0k p", "% =%% x")
}

func TestArgsHas(t *testing.T) {
	var a Args

	// single arg
	testArgsHas(t, &a, "foo", "foo")
	testArgsHasNot(t, &a, "foo", "bar", "baz", "")

	// multi args without values
	testArgsHas(t, &a, "foo&bar", "foo", "bar")
	testArgsHasNot(t, &a, "foo&bar", "", "aaaa")

	// multi args
	testArgsHas(t, &a, "b=xx&=aaa&c=", "b", "", "c")
	testArgsHasNot(t, &a, "b=xx&=aaa&c=", "xx", "aaa", "foo")

	// encoded args
	testArgsHas(t, &a, "a+b=c+d%20%20e", "a b")
	testArgsHasNot(t, &a, "a+b=c+d", "a+b", "c+d")
}

func testArgsHas(t *testing.T, a *Args, s string, expectedKeys ...string) {
	a.Parse(s)
	for _, key := range expectedKeys {
		if !a.Has(key) {
			t.Fatalf("Missing key %q in %q", key, s)
		}
	}
}

func testArgsHasNot(t *testing.T, a *Args, s string, unexpectedKeys ...string) {
	a.Parse(s)
	for _, key := range unexpectedKeys {
		if a.Has(key) {
			t.Fatalf("Unexpected key %q in %q", key, s)
		}
	}
}

func testArgsParse(t *testing.T, a *Args, s string, expectedLen int, expectedArgs ...string) {
	a.Parse(s)
	if a.Len() != expectedLen {
		t.Fatalf("Unexpected args len %d. Expected %d. s=%q", a.Len(), expectedLen, s)
	}
	for _, xx := range expectedArgs {
		tmp := strings.SplitN(xx, "=", 2)
		k := tmp[0]
		v := tmp[1]
		buf := a.Peek(k)
		if string(buf) != v {
			t.Fatalf("Unexpected value for key=%q: %q. Expected %q. s=%q", k, buf, v, s)
		}
	}
}
