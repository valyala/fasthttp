// Package fasthttpadaptor provides helper functions for converting net/http
// request handlers to fasthttp request handlers.
package fasthttpadaptor

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

// NewFastHTTPHandlerFunc wraps net/http handler func to fasthttp
// request handler, so it can be passed to fasthttp server.
//
// While this function may be used for easy switching from net/http to fasthttp,
// it has the following drawbacks comparing to using manually written fasthttp
// request handler:
//
//   - A lot of useful functionality provided by fasthttp is missing
//     from net/http handler.
//   - net/http -> fasthttp handler conversion has some overhead,
//     so the returned handler will be always slower than manually written
//     fasthttp handler.
//
// So it is advisable using this function only for quick net/http -> fasthttp
// switching. Then manually convert net/http handlers to fasthttp handlers
// according to https://github.com/valyala/fasthttp#switching-from-nethttp-to-fasthttp .
func NewFastHTTPHandlerFunc(h http.HandlerFunc) fasthttp.RequestHandler {
	return NewFastHTTPHandler(h)
}

// NewFastHTTPHandler wraps net/http handler to fasthttp request handler,
// so it can be passed to fasthttp server.
//
// While this function may be used for easy switching from net/http to fasthttp,
// it has the following drawbacks comparing to using manually written fasthttp
// request handler:
//
//   - A lot of useful functionality provided by fasthttp is missing
//     from net/http handler.
//   - net/http -> fasthttp handler conversion has some overhead,
//     so the returned handler will be always slower than manually written
//     fasthttp handler.
//
// So it is advisable using this function only for quick net/http -> fasthttp
// switching. Then manually convert net/http handlers to fasthttp handlers
// according to https://github.com/valyala/fasthttp#switching-from-nethttp-to-fasthttp .
func NewFastHTTPHandler(h http.Handler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		var r http.Request
		if err := ConvertRequest(ctx, &r, true); err != nil {
			ctx.Logger().Printf("cannot parse requestURI %q: %v", r.RequestURI, err)
			ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
			return
		}

		w := acquireWriter(ctx)
		// Serve the net/http handler concurrently so we can react to Flush/Hijack.
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					// ErrAbortHandler is net/http's quiet give-up.
					mode := modePanicked
					if rec == http.ErrAbortHandler {
						mode = modeAborted
					}
					if w.pw != nil {
						// The stream is the server's now: the exit is recorded
						// while the request is still ours, then the pipe error
						// cuts the connection.
						w.stream.fail(ctx, rec)
						_ = w.pw.CloseWithError(io.ErrUnexpectedEOF)
					} else if mode == modePanicked {
						ctx.Logger().Printf("panic in net/http handler: %v", rec)
					}

					select {
					case w.modeCh <- mode:
					default:
					}
				} else {
					// Signal completion if no other mode was selected yet.
					select {
					case w.modeCh <- modeDone:
					default:
					}
				}

				_ = w.Close()
			}()

			h.ServeHTTP(w, r.WithContext(ctx))
		}()

		// Decide mode by first event.
		switch <-w.modeCh {
		case modeDone:
			// Buffered, no Flush() nor Hijack().
			ctx.SetStatusCode(w.status())
			haveContentType := false
			// The framing is the one committed with the header, or the final
			// header map's when the handler never committed one.
			f := w.frame
			if !w.committed {
				f = frameOf(w.Header())
			}
			carry := carriesTrailers(ctx, f)
			announced := f.announce(ctx, carry)
			for k, vv := range w.Header() {
				if k == fasthttp.HeaderContentType {
					haveContentType = true
				}
				if isTrailerField(k, announced) {
					continue
				}

				for _, v := range vv {
					ctx.Response.Header.Add(k, v)
				}
			}

			if !haveContentType {
				// From net/http.ResponseWriter.Write:
				// If the Header does not contain a Content-Type line, Write adds a Content-Type set
				// to the result of passing the initial 512 bytes of written data to DetectContentType.
				l := min(len(w.responseBody), 512)
				if l > 0 {
					ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(w.responseBody[:l]))
				}
			}
			// An announcement or a TrailerPrefix field committed with the
			// header makes the body chunked to carry the trailers, as does a
			// body net/http's buffer cannot hold or a chunked
			// Transfer-Encoding; a prefix field added late to a smaller body
			// is dropped.
			late := f.chunked || !w.committed || len(w.responseBody) > bufferBeforeChunkingSize
			if carry && (announced != nil || f.prefix || (late && hasTrailerPrefix(w.Header()))) {
				// The trailers register once the header block is written, as
				// for a flushed response.
				w.stream = &streamBody{pre: w.consumePreflush(), w: w, announced: announced, trailers: true}
				ctx.Response.SetBodyStream(w.stream, -1)
				return
			}
			if len(w.responseBody) > 0 {
				ctx.Response.SetBody(w.responseBody)
			}
			releaseWriter(w)

		case modeFlushed:
			// Streaming: send the headers now and hand the body to SetBodyStream.
			ctx.SetStatusCode(w.status())

			haveContentType := false
			f := w.frame
			if !w.committed {
				f = frameOf(w.Header())
			}
			carry := carriesTrailers(ctx, f)
			announced := f.announce(ctx, carry)
			for k, vv := range w.Header() {
				// No Content-Length when streaming.
				if k == fasthttp.HeaderContentLength {
					continue
				}
				if k == fasthttp.HeaderContentType {
					haveContentType = true
				}
				if isTrailerField(k, announced) {
					continue
				}
				for _, v := range vv {
					ctx.Response.Header.Add(k, v)
				}
			}
			if !haveContentType {
				w.mu.Lock()
				if len(w.responseBody) > 0 {
					l := min(len(w.responseBody), 512)
					ctx.Response.Header.Set(fasthttp.HeaderContentType, http.DetectContentType(w.responseBody[:l]))
				}
				w.mu.Unlock()
			}

			// The pipe feeds fasthttp's chunked writer directly, so the
			// server truncates the response and drops the connection when a
			// handler exit closes the pipe with an error.
			w.stream = &streamBody{pre: w.consumePreflush(), w: w, announced: announced, trailers: carry}
			ctx.Response.SetBodyStream(w.stream, -1)
			// Pre-flush bytes carry the headers out. Without any, or with a
			// body the server skips, flush them alone.
			ctx.Response.ImmediateHeaderFlush = len(w.stream.pre) == 0 || bodySkipped(ctx)

			// Signal the writer that streaming is ready so Flush() can return.
			close(w.streamReady)

		case modeHijacked:
			releaseWriter(w)
			return

		case modeAborted:
			releaseWriter(w)
			// Hijacking skips the response write and leaves teardown to the
			// server; the callback's Close only acts under KeepHijackedConns.
			ctx.HijackSetNoResponse(true)
			ctx.Hijack(func(c net.Conn) { _ = c.Close() })

		case modePanicked:
			panic("net/http handler panicked")
		}
	}
}

// streamBody feeds a flushed response, or a buffered one that carries
// trailers, through fasthttp's chunked writer. It registers the handler's
// trailers when the stream ends cleanly and recycles the writer once the
// server is done with it.
type streamBody struct {
	mu        sync.Mutex
	pre       []byte // pooled pre-flush bytes, recycled by Close
	w         *writer
	announced map[string]bool
	err       error
	trailers  bool // register the trailers at EOF
	trailered bool
	closed    bool
}

func (b *streamBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if len(b.pre) > 0 {
		n := copy(p, b.pre)
		b.pre = b.pre[n:]
		b.mu.Unlock()
		return n, nil
	}
	b.mu.Unlock()
	// The pipe is read without the lock, so Close and fail never wait on it.
	// A buffered body has no pipe: nothing follows the pre-flush bytes.
	n, err := 0, io.EOF
	if b.w.pr != nil {
		n, err = b.w.pr.Read(p)
	}
	if err == io.EOF {
		// The handler has returned, so its trailer values are final; a stream
		// the server closed may belong to a response already reset.
		b.mu.Lock()
		if b.trailers && !b.closed && !b.trailered {
			b.trailered = true
			writeTrailers(b.w.ctx, b.w.Header(), b.announced)
		}
		b.mu.Unlock()
	}
	return n, err
}

// fail records a handler exit while the server still owns the request; a
// closed stream may belong to a request already released or reused.
func (b *streamBody) fail(ctx *fasthttp.RequestCtx, rec any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.err = io.ErrUnexpectedEOF
	if rec != http.ErrAbortHandler {
		ctx.Logger().Printf("panic in net/http handler: %v", rec)
	}
}

// Close fails further reads before the pre-flush buffer is recycled, since a
// discarded body may still be read by a compression goroutine, and reports
// a handler exit the server has not read.
func (b *streamBody) Close() error {
	if b.w.pr != nil {
		_ = b.w.pr.Close()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		releaseWriter(b.w)
	}
	return b.err
}

// CloseWithError reports the handler's exit when CompressHandler's goroutine,
// not the server, closes the stream.
func (b *streamBody) CloseWithError(error) error {
	return b.Close()
}

var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

const (
	modeDone = iota + 1
	modeFlushed
	modeHijacked
	modePanicked
	modeAborted
)

// Writer implements http.ResponseWriter + http.Flusher + http.Hijacker +
// SetReadDeadline/SetWriteDeadline (for http.ResponseController) for the adaptor.
type writer struct {
	ctx        *fasthttp.RequestCtx
	h          http.Header
	statusCode atomic.Int64

	mu           sync.Mutex
	responseBody []byte
	bufPool      *[]byte

	pr     *io.PipeReader
	stream *streamBody
	pw     *io.PipeWriter

	// frame is the framing committed with the header, at WriteHeader or the
	// first Write, when net/http fixes it.
	committed bool
	frame     framing

	hijacked atomic.Bool

	modeCh chan int

	streamReady chan struct{}

	flushOnce sync.Once
	closeOnce sync.Once
}

func acquireWriter(ctx *fasthttp.RequestCtx) *writer {
	return &writer{
		ctx:         ctx,
		h:           make(http.Header),
		modeCh:      make(chan int, 1),
		streamReady: make(chan struct{}),
	}
}

func releaseWriter(w *writer) {
	_ = w.Close()
	if w.bufPool != nil {
		bufferPool.Put(w.bufPool)
		w.bufPool = nil
	}
}

func (w *writer) Header() http.Header {
	return w.h
}

func (w *writer) WriteHeader(code int) {
	// Allow the same codes as net/http.
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %v", code))
	}
	if w.statusCode.CompareAndSwap(0, int64(code)) {
		w.commit()
	}
}

func (w *writer) commit() {
	if !w.committed {
		w.committed = true
		w.frame = frameOf(w.h)
	}
}

func (w *writer) Write(p []byte) (int, error) {
	select {
	case <-w.streamReady:
		return w.pw.Write(p)
	default:
	}

	w.mu.Lock()
	select {
	case <-w.streamReady:
		w.mu.Unlock()
		return w.pw.Write(p)
	default:
	}
	defer w.mu.Unlock()

	if w.responseBody == nil {
		w.bufPool = bufferPool.Get().(*[]byte) //nolint:forcetypeassert
		w.responseBody = (*w.bufPool)[:0]
		w.commit()
	}
	w.responseBody = append(w.responseBody, p...)
	return len(p), nil
}

func (w *writer) Flush() {
	if w.hijacked.Load() {
		return
	}
	w.flushOnce.Do(func() {
		w.pr, w.pw = io.Pipe()
		select {
		case w.modeCh <- modeFlushed:
		default:
		}
	})
	<-w.streamReady
}

type wrappedConn struct {
	net.Conn

	wg   sync.WaitGroup
	once sync.Once
}

func (c *wrappedConn) Close() (err error) {
	c.once.Do(func() {
		err = c.Conn.Close()
		c.wg.Done()
	})
	return err
}

func (w *writer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.hijacked.CompareAndSwap(false, true) {
		return nil, nil, http.ErrHijacked
	}

	// Tell fasthttp not to send any HTTP response before hijacking.
	w.ctx.HijackSetNoResponse(true)

	conn := &wrappedConn{Conn: w.ctx.Conn()}
	conn.wg.Add(1)
	w.ctx.Hijack(func(net.Conn) {
		conn.wg.Wait()
	})

	bufW := bufio.NewWriter(conn)

	// Write any unflushed body to the hijacked connection buffer.
	unflushedBody := w.consumePreflush()
	if len(unflushedBody) > 0 {
		if _, err := bufW.Write(unflushedBody); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}

	select {
	case w.modeCh <- modeHijacked:
	default:
	}

	return conn, &bufio.ReadWriter{Reader: bufio.NewReader(conn), Writer: bufW}, nil
}

func (w *writer) Close() error {
	w.closeOnce.Do(func() {
		if w.pw != nil {
			_ = w.pw.Close()
		}
	})
	return nil
}

// status returns the effective status code (defaults to 200).
func (w *writer) status() int {
	code := int(w.statusCode.Load())
	if code == 0 {
		// No WriteHeader was called; check if ctx already has a status code set
		// by a caller before NewFastHTTPHandler ran.
		if ctxCode := w.ctx.Response.StatusCode(); ctxCode != 0 {
			return ctxCode
		}
		return http.StatusOK
	}
	return code
}

// consumePreflush returns pre-flush bytes and clears the buffer.
func (w *writer) consumePreflush() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.responseBody) == 0 {
		return nil
	}
	out := w.responseBody
	w.responseBody = nil
	return out
}

// bodySkipped reports whether the server will write no body for ctx.
func bodySkipped(ctx *fasthttp.RequestCtx) bool {
	status := ctx.Response.StatusCode()
	return ctx.IsHead() || ctx.Response.SkipBody ||
		status < 200 || status == http.StatusNoContent || status == http.StatusNotModified
}

// bufferBeforeChunkingSize is net/http's: a body it cannot hold is sent
// chunked, and TrailerPrefix fields added late still follow it.
const bufferBeforeChunkingSize = 2048

// framing is what the handler's headers decide about the body when net/http
// commits them: fixed is a valid Content-Length or an identity
// Transfer-Encoding, which carry no trailers; chunked is any other
// Transfer-Encoding, which overrides a Content-Length, or a Content-Length
// that is no length, which net/http chunks around; prefix is a TrailerPrefix
// field; trailer is the announcement and values what the announced names
// held then.
type framing struct {
	fixed, chunked, prefix bool
	trailer                []string
	values                 map[string][]string
}

func frameOf(h http.Header) framing {
	te := h.Get(fasthttp.HeaderTransferEncoding)
	f := framing{
		fixed:   te == "identity",
		chunked: te != "" && te != "identity",
		prefix:  hasTrailerPrefix(h),
		trailer: slices.Clone(h[fasthttp.HeaderTrailer]),
	}
	if _, ok := h[fasthttp.HeaderContentLength]; ok && !f.fixed && !f.chunked {
		if n, err := strconv.ParseInt(h.Get(fasthttp.HeaderContentLength), 10, 64); err == nil && n >= 0 {
			f.fixed = true
		} else {
			f.chunked = true
		}
	}
	for _, list := range f.trailer {
		for name := range strings.SplitSeq(list, ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if vv := h[name]; len(vv) > 0 {
				if f.values == nil {
					f.values = make(map[string][]string)
				}
				f.values[name] = slices.Clone(vv)
			}
		}
	}
	return f
}

// hasTrailerPrefix reports whether h carries a TrailerPrefix field.
func hasTrailerPrefix(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			return true
		}
	}
	return false
}

// carriesTrailers reports whether the response can have a trailer section:
// not with a fixed framing, not for an HTTP/1.0 client, and not without a
// body.
func carriesTrailers(ctx *fasthttp.RequestCtx, f framing) bool {
	return !f.fixed && string(ctx.Request.Header.Protocol()) != "HTTP/1.0" && !bodySkipped(ctx)
}

// announce registers the committed announcement on the response and returns
// the announced names. Without a trailer section the names keep the values
// committed with the header, as net/http's header block does, and lose the
// rest.
func (f framing) announce(ctx *fasthttp.RequestCtx, carry bool) map[string]bool {
	announced := announceTrailers(ctx, f.trailer)
	if !carry {
		ctx.Response.Header.Del(fasthttp.HeaderTrailer)
		for name := range announced {
			for _, v := range f.values[name] {
				ctx.Response.Header.Add(name, v)
			}
		}
	}
	return announced
}

// announceTrailers registers the handler's Trailer header values on the
// response and returns the names fasthttp accepted; a forbidden name stays a
// header, as it does in net/http.
func announceTrailers(ctx *fasthttp.RequestCtx, lists []string) map[string]bool {
	for _, list := range lists {
		_ = ctx.Response.Header.AddTrailer(list)
	}
	var names map[string]bool
	for _, name := range ctx.Response.Header.PeekTrailerKeys() {
		if names == nil {
			names = make(map[string]bool)
		}
		names[http.CanonicalHeaderKey(string(name))] = true
	}
	return names
}

// isTrailerField reports whether a header map entry stays out of the header
// block: a trailer, or the announcement already registered.
func isTrailerField(key string, announced map[string]bool) bool {
	return key == fasthttp.HeaderTrailer || strings.HasPrefix(key, http.TrailerPrefix) || announced[key]
}

// writeTrailers registers the handler's trailers on the response, prefix
// fields before announced names as net/http orders them. It runs once the
// header block is written, so announcing a name leaves an upfront value of
// the same name where it was sent.
func writeTrailers(ctx *fasthttp.RequestCtx, h http.Header, announced map[string]bool) {
	for key, values := range h {
		if name, ok := strings.CutPrefix(key, http.TrailerPrefix); ok {
			addTrailer(ctx, http.CanonicalHeaderKey(name), values)
		}
	}
	for key, values := range h {
		if announced[key] {
			addTrailer(ctx, key, values)
		}
	}
}

func addTrailer(ctx *fasthttp.RequestCtx, name string, values []string) {
	if ctx.Response.Header.AddTrailer(name) != nil {
		return
	}
	for _, v := range values {
		ctx.Response.Header.Add(name, v)
	}
}

// SetReadDeadline sets the read deadline on the underlying connection.
// This enables support for http.ResponseController.SetReadDeadline.
func (w *writer) SetReadDeadline(deadline time.Time) error {
	if w.ctx == nil {
		return fasthttp.ErrNilConnection
	}
	return w.ctx.SetReadDeadline(deadline)
}

// SetWriteDeadline sets the write deadline on the underlying connection.
// This enables support for http.ResponseController.SetWriteDeadline.
func (w *writer) SetWriteDeadline(deadline time.Time) error {
	if w.ctx == nil {
		return fasthttp.ErrNilConnection
	}
	return w.ctx.SetWriteDeadline(deadline)
}

// SetDeadline sets the read and write deadlines on the underlying connection.
func (w *writer) SetDeadline(deadline time.Time) error {
	if w.ctx == nil {
		return fasthttp.ErrNilConnection
	}
	return w.ctx.SetDeadline(deadline)
}
