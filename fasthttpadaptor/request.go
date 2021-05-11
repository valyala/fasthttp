package fasthttpadaptor

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/valyala/fasthttp"
)

// ConvertRequest convert a fasthttp.Request to http.Request
// setURI is optionally because your use in client mode
func ConvertRequest(ctx *fasthttp.RequestCtx, setURI bool) (*http.Request, error) {
	var r http.Request

	r.Method = string(ctx.Method())
	r.Proto = "HTTP/1.1"
	r.ProtoMajor = 1
	r.ProtoMinor = 1
	r.ContentLength = int64(len(ctx.PostBody()))
	r.RemoteAddr = ctx.RemoteAddr().String()
	r.Host = string(ctx.Host())

	if setURI {
		r.RequestURI = string(ctx.RequestURI())
	}

	hdr := make(http.Header)
	ctx.Request.Header.VisitAll(func(k, v []byte) {
		sk := string(k)
		sv := string(v)
		switch sk {
		case "Transfer-Encoding":
			r.TransferEncoding = append(r.TransferEncoding, sv)
		default:
			hdr.Set(sk, sv)
		}
	})

	r.Header = hdr
	r.Body = ioutil.NopCloser(bytes.NewReader(ctx.PostBody()))

	rURL, err := url.ParseRequestURI(string(ctx.RequestURI()))
	if err != nil {
		return nil, err
	}

	r.URL = rURL
	return &r, nil
}
