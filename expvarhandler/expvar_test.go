package expvarhandler

import (
	"encoding/json"
	"expvar"
	"strings"
	"sync"
	"testing"

	"github.com/valyala/fasthttp"
)

var once sync.Once

func TestExpvarHandlerBasic(t *testing.T) {
	t.Parallel()

	// Publish panics if the same var is published more than once,
	// which can happen if the test is run with -count
	once.Do(func() {
		expvar.Publish("customVar", expvar.Func(func() any {
			return "foobar"
		}))
	})

	var ctx fasthttp.RequestCtx

	expvarHandlerCalls.Set(0)

	ExpvarHandler(&ctx)

	body := ctx.Response.Body()

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := m["cmdline"]; !ok {
		t.Fatalf("cannot locate cmdline expvar")
	}
	if _, ok := m["memstats"]; !ok {
		t.Fatalf("cannot locate memstats expvar")
	}

	v := m["customVar"]
	sv, ok := v.(string)
	if !ok {
		t.Fatalf("unexpected custom var type %T. Expecting string", v)
	}
	if sv != "foobar" {
		t.Fatalf("unexpected custom var value: %q. Expecting %q", v, "foobar")
	}

	v = m["expvarHandlerCalls"]
	fv, ok := v.(float64)
	if !ok {
		t.Fatalf("unexpected expvarHandlerCalls type %T. Expecting float64", v)
	}
	if int(fv) != 1 {
		t.Fatalf("unexpected value for expvarHandlerCalls: %v. Expecting %v", fv, 1)
	}
}

func TestExpvarHandlerRegexp(t *testing.T) {
	var ctx fasthttp.RequestCtx
	ctx.QueryArgs().Set("r", "cmd")
	ExpvarHandler(&ctx)
	body := string(ctx.Response.Body())
	if !strings.Contains(body, `"cmdline"`) {
		t.Fatalf("missing 'cmdline' expvar")
	}
	if strings.Contains(body, `"memstats"`) {
		t.Fatalf("unexpected memstats expvar found")
	}
}
