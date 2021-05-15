package fasthttpadaptor

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/valyala/fasthttp"
)

// ConvertRequest convert a fasthttp.Request to an http.Request
// forServer should be set to true when the http.Request is going to passed to a http.Handler.
func ConvertRequest(ctx *fasthttp.RequestCtx, r *http.Request, forServer bool) error {
	rURL, err := url.ParseRequestURI(string(ctx.RequestURI()))
	if err != nil {
		return err
	}

	r.Method = string(ctx.Method())
	r.Proto = "HTTP/1.1"
	r.ProtoMajor = 1
	r.ProtoMinor = 1
	r.ContentLength = int64(len(ctx.PostBody()))
	r.RemoteAddr = ctx.RemoteAddr().String()
	r.Host = string(ctx.Host())

	if forServer {
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
	r.URL = rURL
	return nil
}
