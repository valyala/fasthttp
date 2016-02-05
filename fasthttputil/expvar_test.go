package fasthttputil

import (
	"encoding/json"
	"expvar"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestExpvarHandler(t *testing.T) {
	expvar.Publish("customVar", expvar.Func(func() interface{} {
		return "foobar"
	}))

	var ctx fasthttp.RequestCtx

	ExpvarHandler(&ctx)

	body := ctx.Response.Body()

	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if _, ok := m["cmdline"]; !ok {
		t.Fatalf("cannot locate cmdline expvar")
	}
	if _, ok := m["memstats"]; !ok {
		t.Fatalf("cannot locate memstats expvar")
	}

	v, ok := m["customVar"]
	if !ok {
		t.Fatalf("cannot locate customVar")
	}
	if v != "foobar" {
		t.Fatalf("unexpected custom var value: %q. Expecting %q", v, "foobar")
	}
}
