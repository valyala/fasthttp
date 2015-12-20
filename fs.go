package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// PathRewriteFunc must return new request path based on arbitrary ctx
// info such as ctx.Path().
//
// Path rewriter is used in FS for translating the current request
// to the local filesystem path relative to FS.Root.
//
// The returned path may refer to ctx members. For example, ctx.Path().
type PathRewriteFunc func(ctx *RequestCtx) []byte

// NewPathSlashesStripper returns path rewriter, which strips slashesCount
// leading slashes from the path.
//
// Examples:
//
//   * slashesCount = 0, original path: "/foo/bar", result: "/foo/bar"
//   * slashesCount = 1, original path: "/foo/bar", result: "/bar"
//   * slashesCount = 2, original path: "/foo/bar", result: ""
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathSlashesStripper(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		return stripLeadingSlashes(ctx.Path(), slashesCount)
	}
}

// NewPathPrefixStripper returns path rewriter, which removes prefixSize bytes
// from the path prefix.
//
// Examples:
//
//   * prefixSize = 0, original path: "/foo/bar", result: "/foo/bar"
//   * prefixSize = 3, original path: "/foo/bar", result: "o/bar"
//   * prefixSize = 7, original path: "/foo/bar", result: "r"
//
// The returned path rewriter may be used as FS.PathRewrite .
func NewPathPrefixStripper(prefixSize int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		path := ctx.Path()
		if len(path) >= prefixSize {
			path = path[prefixSize:]
		}
		return path
	}
}

// FS represents settings for request handler serving static files
// from the local filesystem.
type FS struct {
	// Path to the root directory to serve files from.
	Root string

	// Index pages for directories without index.html are automatically
	// generated if set.
	//
	// By default index pages aren't generated.
	GenerateIndexPages bool

	// Path rewriting function.
	//
	// By default request path is not modified.
	PathRewrite PathRewriteFunc

	// The duration for files' caching.
	//
	// FSHandlerCacheDuration is used by default.
	CacheDuration time.Duration

	started bool
}

// FSHandlerCacheDuration is the duration for caching open file handles
// by FSHandler.
const FSHandlerCacheDuration = 5 * time.Second

// FSHandler returns request handler serving static files from
// the given root folder.
//
// stripSlashes indicates how many leading slashes must be stripped
// from requested path before searching requested file in the root folder.
// Examples:
//
//   * stripSlashes = 0, original path: "/foo/bar", result: "/foo/bar"
//   * stripSlashes = 1, original path: "/foo/bar", result: "/bar"
//   * stripSlashes = 2, original path: "/foo/bar", result: ""
//
// The returned request handler automatically generates index pages
// for directories without index.html.
//
// The returned handler caches requested file handles
// for FSHandlerCacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if root folder contains many files.
//
// Do not create multiple request handler instances for the same
// (root, stripSlashes) arguments - just reuse a single instance.
// Otherwise goroutine leak will occur.
func FSHandler(root string, stripSlashes int) RequestHandler {
	fs := &FS{
		Root:               root,
		GenerateIndexPages: true,
		PathRewrite:        NewPathSlashesStripper(stripSlashes),
	}
	return fs.NewRequestHandler()
}

// NewRequestHandler returns new request handler with the given FS settings.
//
// The returned handler caches requested file handles
// for FS.CacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if FS.Root folder contains many files.
//
// Do not create multiple request handlers from a single FS instance -
// just reuse a single request handler.
func (fs *FS) NewRequestHandler() RequestHandler {
	if fs.started {
		panic("BUG: NewRequestHandler() cannot be called multiple times for the same FS instance")
	}
	fs.started = true

	root := fs.Root

	// strip trailing slashes from the root path
	for len(root) > 0 && root[len(root)-1] == '/' {
		root = root[:len(root)-1]
	}

	// serve files from the current working directory if root is empty
	if len(root) == 0 {
		root = "."
	}

	cacheDuration := fs.CacheDuration
	if cacheDuration <= 0 {
		cacheDuration = FSHandlerCacheDuration
	}

	pathRewrite := fs.PathRewrite
	if pathRewrite == nil {
		pathRewrite = NewPathSlashesStripper(0)
	}

	h := &fsHandler{
		root:               root,
		pathRewrite:        pathRewrite,
		generateIndexPages: fs.GenerateIndexPages,
		cacheDuration:      cacheDuration,
		cache:              make(map[string]*fsFile),
	}

	go func() {
		for {
			time.Sleep(cacheDuration / 2)
			h.cleanCache()
		}
	}()

	return h.handleRequest
}

type fsHandler struct {
	root               string
	pathRewrite        PathRewriteFunc
	generateIndexPages bool
	cacheDuration      time.Duration

	cache        map[string]*fsFile
	pendingFiles []*fsFile
	cacheLock    sync.Mutex

	smallFileReaderPool sync.Pool
}

type fsFile struct {
	h             *fsHandler
	f             *os.File
	dirIndex      []byte
	contentType   string
	contentLength int

	lastModified    time.Time
	lastModifiedStr []byte

	t            time.Time
	readersCount int

	bigFiles     []*bigFileReader
	bigFilesLock sync.Mutex
}

func (ff *fsFile) NewReader() (io.Reader, error) {
	if ff.isBig() {
		r, err := ff.bigFileReader()
		if err != nil {
			ff.decReadersCount()
		}
		return r, err
	}
	return ff.smallFileReader(), nil
}

func (ff *fsFile) smallFileReader() io.Reader {
	v := ff.h.smallFileReaderPool.Get()
	if v == nil {
		r := &fsSmallFileReader{
			ff: ff,
		}
		r.v = r
		return r
	}
	r := v.(*fsSmallFileReader)
	r.ff = ff
	if r.offset > 0 {
		panic("BUG: fsSmallFileReader with non-nil offset found in the pool")
	}
	return r
}

// files bigger than this size are sent with sendfile
const maxSmallFileSize = 2 * 4096

func (ff *fsFile) isBig() bool {
	return ff.contentLength > maxSmallFileSize && len(ff.dirIndex) == 0
}

func (ff *fsFile) bigFileReader() (io.Reader, error) {
	if ff.f == nil {
		panic("BUG: ff.f must be non-nil in bigFileReader")
	}

	var r io.Reader

	ff.bigFilesLock.Lock()
	n := len(ff.bigFiles)
	if n > 0 {
		r = ff.bigFiles[n-1]
		ff.bigFiles = ff.bigFiles[:n-1]
	}
	ff.bigFilesLock.Unlock()

	if r != nil {
		return r, nil
	}

	f, err := os.Open(ff.f.Name())
	if err != nil {
		return nil, fmt.Errorf("cannot open already opened file: %s", err)
	}
	return &bigFileReader{
		f:  f,
		ff: ff,
	}, nil
}

func (ff *fsFile) Release() {
	if ff.f != nil {
		ff.f.Close()

		if ff.isBig() {
			ff.bigFilesLock.Lock()
			for _, r := range ff.bigFiles {
				r.f.Close()
			}
			ff.bigFilesLock.Unlock()
		}
	}
}

func (ff *fsFile) decReadersCount() {
	ff.h.cacheLock.Lock()
	ff.readersCount--
	if ff.readersCount < 0 {
		panic("BUG: negative fsFile.readersCount!")
	}
	ff.h.cacheLock.Unlock()
}

// bigFileReader attempts to trigger sendfile
// for sending big files over the wire.
type bigFileReader struct {
	f  *os.File
	ff *fsFile
}

func (r *bigFileReader) Read(p []byte) (int, error) {
	return r.f.Read(p)
}

func (r *bigFileReader) WriteTo(w io.Writer) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		// fast path. Senfile must be triggered
		return rf.ReadFrom(r.f)
	}

	// slow path
	return copyZeroAlloc(w, r.f)
}

func (r *bigFileReader) Close() error {
	n, err := r.f.Seek(0, 0)
	if err == nil {
		if n != 0 {
			panic("BUG: File.Seek(0,0) returned (non-zero, nil)")
		}

		ff := r.ff
		ff.bigFilesLock.Lock()
		ff.bigFiles = append(ff.bigFiles, r)
		ff.bigFilesLock.Unlock()
	} else {
		r.f.Close()
	}
	r.ff.decReadersCount()
	return err
}

type fsSmallFileReader struct {
	ff     *fsFile
	offset int64

	v interface{}
}

func (r *fsSmallFileReader) Close() error {
	ff := r.ff
	ff.decReadersCount()
	r.ff = nil
	r.offset = 0
	ff.h.smallFileReaderPool.Put(r.v)
	return nil
}

func (r *fsSmallFileReader) Read(p []byte) (int, error) {
	ff := r.ff

	if ff.f != nil {
		n, err := ff.f.ReadAt(p, r.offset)
		r.offset += int64(n)
		return n, err
	}

	if r.offset == int64(len(ff.dirIndex)) {
		return 0, io.EOF
	}
	n := copy(p, ff.dirIndex[r.offset:])
	r.offset += int64(n)
	return n, nil
}

func (r *fsSmallFileReader) WriteTo(w io.Writer) (int64, error) {
	if r.offset != 0 {
		panic("BUG: non-zero offset! Read() mustn't be called before WriteTo()")
	}

	ff := r.ff

	var n int
	var err error
	if ff.f != nil {
		if rf, ok := w.(io.ReaderFrom); ok {
			return rf.ReadFrom(r)
		}

		bufv := copyBufPool.Get()
		buf := bufv.([]byte)
		for err != nil {
			n, err = ff.f.ReadAt(buf, r.offset)
			nw, errw := w.Write(buf[:n])
			r.offset += int64(nw)
			if errw == nil && nw != n {
				panic("BUG: Write(p) returned (n, nil), where n != len(p)")
			}
			if err == nil {
				err = errw
			}
		}
		copyBufPool.Put(bufv)

		if err == io.EOF {
			err = nil
		}
		return r.offset, err
	}

	n, err = w.Write(ff.dirIndex)
	r.offset += int64(n)
	return r.offset, err
}

func (h *fsHandler) cleanCache() {
	t := time.Now()
	h.cacheLock.Lock()

	// Close files which couldn't be closed before due to non-zero
	// readers count.
	var pendingFiles []*fsFile
	for _, ff := range h.pendingFiles {
		if ff.readersCount > 0 {
			pendingFiles = append(pendingFiles, ff)
		} else {
			ff.Release()
		}
	}
	h.pendingFiles = pendingFiles

	// Close stale file handles.
	for k, ff := range h.cache {
		if t.Sub(ff.t) > h.cacheDuration {
			if ff.readersCount > 0 {
				// There are pending readers on stale file handle,
				// so we cannot close it. Put it into pendingFiles
				// so it will be closed later.
				h.pendingFiles = append(h.pendingFiles, ff)
			} else {
				ff.Release()
			}
			delete(h.cache, k)
		}
	}

	h.cacheLock.Unlock()
}

func (h *fsHandler) handleRequest(ctx *RequestCtx) {
	path := h.pathRewrite(ctx)
	path = stripTrailingSlashes(path)

	if n := bytes.IndexByte(path, 0); n >= 0 {
		ctx.Logger().Printf("cannot serve path with nil byte at position %d: %q", n, path)
		ctx.Error("Are you a hacker?", StatusBadRequest)
		return
	}

	h.cacheLock.Lock()
	ff, ok := h.cache[string(path)]
	if ok {
		ff.readersCount++
	}
	h.cacheLock.Unlock()

	if !ok {
		pathStr := string(path)
		filePath := h.root + pathStr
		var err error
		ff, err = h.openFSFile(filePath)
		if err == errDirIndexRequired {
			if !h.generateIndexPages {
				ctx.Logger().Printf("An attempt to access directory without index page. Directory %q", filePath)
				ctx.Error("Directory index is forbidden", StatusForbidden)
				return
			}
			ff, err = h.createDirIndex(ctx.URI(), filePath)
			if err != nil {
				ctx.Logger().Printf("Cannot create index for directory %q: %s", filePath, err)
				ctx.Error("Cannot create directory index", StatusNotFound)
				return
			}
		} else if err != nil {
			ctx.Logger().Printf("cannot open file %q: %s", filePath, err)
			ctx.Error("Cannot open requested path", StatusNotFound)
			return
		}

		h.cacheLock.Lock()
		ff1, ok := h.cache[pathStr]
		if !ok {
			h.cache[pathStr] = ff
			ff.readersCount++
		} else {
			ff1.readersCount++
		}
		h.cacheLock.Unlock()

		if ok {
			// The file has been already opened by another
			// goroutine, so close the current file and use
			// the file opened by another goroutine instead.
			ff.Release()
			ff = ff1
		}
	}

	if !ctx.IfModifiedSince(ff.lastModified) {
		ff.decReadersCount()
		ctx.NotModified()
		return
	}

	r, err := ff.NewReader()
	if err != nil {
		ctx.Logger().Printf("cannot obtain file reader for path=%q: %s", path, err)
		ctx.Error("Internal Server Error", StatusInternalServerError)
		return
	}

	ctx.Response.Header.SetCanonical(strLastModified, ff.lastModifiedStr)
	ctx.SetBodyStream(r, ff.contentLength)
	ctx.SetContentType(ff.contentType)
}

var errDirIndexRequired = errors.New("directory index required")

func (h *fsHandler) createDirIndex(base *URI, filePath string) (*fsFile, error) {
	var buf bytes.Buffer
	w := &buf

	basePathEscaped := html.EscapeString(string(base.Path()))
	fmt.Fprintf(w, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI URI
		base.CopyTo(&parentURI)
		parentURI.Update(string(base.Path()) + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		fmt.Fprintf(w, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	fileinfos, err := f.Readdir(0)
	f.Close()
	if err != nil {
		return nil, err
	}

	fm := make(map[string]os.FileInfo, len(fileinfos))
	var filenames []string
	for _, fi := range fileinfos {
		name := fi.Name()
		fm[name] = fi
		filenames = append(filenames, name)
	}

	var u URI
	base.CopyTo(&u)
	u.Update(string(u.Path()) + "/")

	sort.Sort(sort.StringSlice(filenames))
	for _, name := range filenames {
		u.Update(name)
		pathEscaped := html.EscapeString(string(u.Path()))
		fi := fm[name]
		auxStr := "dir"
		className := "dir"
		if !fi.IsDir() {
			auxStr = fmt.Sprintf("file, %d bytes", fi.Size())
			className = "file"
		}
		fmt.Fprintf(w, `<li><a href="%s" class="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, className, html.EscapeString(name), auxStr, fsModTime(fi.ModTime()))
	}

	fmt.Fprintf(w, "</ul></body></html>")
	dirIndex := w.Bytes()

	lastModified := time.Now()
	ff := &fsFile{
		h:               h,
		dirIndex:        dirIndex,
		contentType:     "text/html; charset=utf-8",
		contentLength:   len(dirIndex),
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),
	}
	return ff, nil
}

func (h *fsHandler) openFSFile(filePath string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	fileStat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	if fileStat.IsDir() {
		f.Close()

		indexPath := filePath + "/index.html"
		ff, err := h.openFSFile(indexPath)
		if err == nil {
			return ff, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		return nil, errDirIndexRequired
	}

	n := fileStat.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	ext := fileExtension(filePath)
	contentType := mime.TypeByExtension(ext)

	lastModified := fileStat.ModTime()
	ff := &fsFile{
		h:               h,
		f:               f,
		contentType:     contentType,
		contentLength:   contentLength,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),
	}
	return ff, nil
}

func stripLeadingSlashes(path []byte, stripSlashes int) []byte {
	for stripSlashes > 0 && len(path) > 0 {
		if path[0] != '/' {
			panic("BUG: path must start with slash")
		}
		n := bytes.IndexByte(path[1:], '/')
		if n < 0 {
			path = path[:0]
			break
		}
		path = path[n+1:]
		stripSlashes--
	}
	return path
}

func stripTrailingSlashes(path []byte) []byte {
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

func fileExtension(path string) string {
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}

func fsLastModified(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return zeroTime, err
	}
	fileInfo, err := f.Stat()
	f.Close()
	if err != nil {
		return zeroTime, err
	}
	return fsModTime(fileInfo.ModTime()), nil
}

func fsModTime(t time.Time) time.Time {
	return t.In(gmtLocation).Truncate(time.Second)
}
