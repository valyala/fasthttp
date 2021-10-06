package fasthttpadaptor

import (
	"bytes"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"

	"github.com/valyala/fasthttp"
)

// ConvertFastRequest converts a fasthttp.Request to a http.Request
// forServer should be set to true when the http.Request is going to be passed to a http.Handler.
func ConvertFastRequest(ctx *fasthttp.RequestCtx, r *http.Request, forServer bool) error {
	body := ctx.PostBody()
	strRequestURI := string(ctx.RequestURI())

	rURL, err := url.ParseRequestURI(strRequestURI)
	if err != nil {
		return err
	}

	r.Method = string(ctx.Method())
	r.Proto = "HTTP/1.1"
	r.ProtoMajor = 1
	r.ProtoMinor = 1
	r.ContentLength = int64(len(body))
	r.RemoteAddr = ctx.RemoteAddr().String()
	r.Host = string(ctx.Host())
	r.TLS = ctx.TLSConnectionState()
	r.Body = ioutil.NopCloser(bytes.NewReader(body))
	r.URL = rURL

	if forServer {
		r.RequestURI = strRequestURI
	}

	if r.Header == nil {
		r.Header = make(http.Header)
	} else if len(r.Header) > 0 {
		for k := range r.Header {
			delete(r.Header, k)
		}
	}

	ctx.Request.Header.VisitAll(func(k, v []byte) {
		sk := string(k)
		sv := string(v)

		switch sk {
		case "Transfer-Encoding":
			r.TransferEncoding = append(r.TransferEncoding, sv)
		default:
			r.Header.Set(sk, sv)
		}
	})

	return nil
}

// ConvertHTTPRequest converts a http.Request to a fasthttp.Request
// forServer should be set to true when the http.Request is going to be passed to a http.Handler.
func ConvertHTTPRequest(r *http.Request, ctx *fasthttp.RequestCtx) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return err
	}

	ip, err := net.ResolveIPAddr("tcp", host)
	if err != nil {
		return err
	}

	ctx.SetRemoteAddr(ip)

	if ctx.Request.Header.Len() > 0 {
		ctx.Request.Header.Reset()
	}

	for k, v := range r.Header {
		if len(v) > 1 {
			for _, vv := range v {
				ctx.Request.Header.Add(k, vv)
			}
		} else {
			ctx.Request.Header.Set(k, v[0])
		}
	}

	if len(r.TransferEncoding) > 0 {
		if len(r.TransferEncoding) > 1 {
			for _, e := range r.TransferEncoding {
				ctx.Request.Header.Add("Transfer-Encoding", e)
			}
		} else {
			ctx.Request.Header.Add("Transfer-Encoding", r.TransferEncoding[0])
		}
	}

	ctx.Request.URI().Update(r.URL.String())

	ctx.Request.Header.SetMethod(r.Method)
	ctx.Request.Header.SetProtocol(r.Proto)
	ctx.Request.Header.SetContentLength(int(r.ContentLength))
	ctx.Request.Header.SetHost(r.Host)
	*ctx.TLSConnectionState() = *r.TLS

	bodyBytes := new(bytes.Buffer)
	_, err = bodyBytes.ReadFrom(r.Body)
	if err != nil {
		return err
	}

	ctx.Request.SetBodyRaw(bodyBytes.Bytes())

	return nil
}
