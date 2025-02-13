package pprofhandler

import (
    "testing"
    "github.com/valyala/fasthttp"
)


// Test generated using Keploy
func TestPprofHandler_Cmdline(t *testing.T) {
    ctx := &fasthttp.RequestCtx{}
    ctx.Request.SetRequestURI("/debug/pprof/cmdline")
    PprofHandler(ctx)
    if ctx.Response.StatusCode() != fasthttp.StatusOK {
        t.Errorf("Expected status code %d, got %d", fasthttp.StatusOK, ctx.Response.StatusCode())
    }
}


// Test generated using Keploy
func TestPprofHandler_Symbol(t *testing.T) {
    ctx := &fasthttp.RequestCtx{}
    ctx.Request.SetRequestURI("/debug/pprof/symbol")
    PprofHandler(ctx)
    if ctx.Response.StatusCode() != fasthttp.StatusOK {
        t.Errorf("Expected status code %d, got %d", fasthttp.StatusOK, ctx.Response.StatusCode())
    }
}


// Test generated using Keploy
func TestPprofHandler_Default(t *testing.T) {
    ctx := &fasthttp.RequestCtx{}
    ctx.Request.SetRequestURI("/debug/pprof/")
    PprofHandler(ctx)
    if ctx.Response.StatusCode() != fasthttp.StatusOK {
        t.Errorf("Expected status code %d, got %d", fasthttp.StatusOK, ctx.Response.StatusCode())
    }
}


// Test generated using Keploy
func TestPprofHandler_CustomProfile(t *testing.T) {
    ctx := &fasthttp.RequestCtx{}
    ctx.Request.SetRequestURI("/debug/pprof/heap")
    PprofHandler(ctx)
    if ctx.Response.StatusCode() != fasthttp.StatusOK {
        t.Errorf("Expected status code %d, got %d", fasthttp.StatusOK, ctx.Response.StatusCode())
    }
}
