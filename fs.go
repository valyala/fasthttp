package fasthttp

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/valyala/bytebufferpool"
)

// ServeFileBytesUncompressed returns HTTP response containing file contents
// from the given path.
//
// Directory contents is returned if path points to directory.
//
// ServeFileBytes may be used for saving network traffic when serving files
// with good compression ratio.
//
// See also RequestCtx.SendFileBytes.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileBytesUncompressed(ctx *RequestCtx, path []byte) {
	ServeFileUncompressed(ctx, b2s(path))
}

// ServeFileUncompressed returns HTTP response containing file contents
// from the given path.
//
// Directory contents is returned if path points to directory.
//
// ServeFile may be used for saving network traffic when serving files
// with good compression ratio.
//
// See also RequestCtx.SendFile.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileUncompressed(ctx *RequestCtx, path string) {
	ctx.Request.Header.DelBytes(strAcceptEncoding)
	ServeFile(ctx, path)
}

// ServeFileBytes returns HTTP response containing compressed file contents
// from the given path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileBytesUncompressed is you don't need serving compressed
// file contents.
//
// See also RequestCtx.SendFileBytes.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFileBytes(ctx *RequestCtx, path []byte) {
	ServeFile(ctx, b2s(path))
}

// ServeFile returns HTTP response containing compressed file contents
// from the given path.
//
// HTTP response may contain uncompressed file contents in the following cases:
//
//   - Missing 'Accept-Encoding: gzip' request header.
//   - No write access to directory containing the file.
//
// Directory contents is returned if path points to directory.
//
// Use ServeFileUncompressed is you don't need serving compressed file contents.
//
// See also RequestCtx.SendFile.
//
// WARNING: do not pass any user supplied paths to this function!
// WARNING: if path is based on user input users will be able to request
// any file on your filesystem! Use fasthttp.FS with a sane Root instead.
func ServeFile(ctx *RequestCtx, path string) {
	rootFSOnce.Do(func() {
		rootFSHandler = rootFS.NewRequestHandler()
	})

	if len(path) == 0 || !filepath.IsAbs(path) {
		// extend relative path to absolute path
		hasTrailingSlash := len(path) > 0 && (path[len(path)-1] == '/' || path[len(path)-1] == '\\')

		var err error
		path = filepath.FromSlash(path)
		if path, err = filepath.Abs(path); err != nil {
			ctx.Logger().Printf("cannot resolve path %q to absolute file path: %v", path, err)
			ctx.Error("Internal Server Error", StatusInternalServerError)
			return
		}
		if hasTrailingSlash {
			path += "/"
		}
	}

	// convert the path to forward slashes regardless the OS in order to set the URI properly
	// the handler will convert back to OS path separator before opening the file
	path = filepath.ToSlash(path)

	ctx.Request.SetRequestURI(path)
	rootFSHandler(ctx)
}

var (
	rootFSOnce sync.Once
	rootFS     = &FS{
		Root:               "",
		AllowEmptyRoot:     true,
		GenerateIndexPages: true,
		Compress:           true,
		CompressBrotli:     true,
		AcceptByteRange:    true,
	}
	rootFSHandler RequestHandler
)

// PathRewriteFunc must return new request path based on arbitrary ctx
// info such as ctx.Path().
//
// Path rewriter is used in FS for translating the current request
// to the local filesystem path relative to FS.Root.
//
// The returned path must not contain '/../' substrings due to security reasons,
// since such paths may refer files outside FS.Root.
//
// The returned path may refer to ctx members. For example, ctx.Path().
type PathRewriteFunc func(ctx *RequestCtx) []byte

// NewVHostPathRewriter returns path rewriter, which strips slashesCount
// leading slashes from the path and prepends the path with request's host,
// thus simplifying virtual hosting for static files.
//
// Examples:
//
//   - host=foobar.com, slashesCount=0, original path="/foo/bar".
//     Resulting path: "/foobar.com/foo/bar"
//
//   - host=img.aaa.com, slashesCount=1, original path="/images/123/456.jpg"
//     Resulting path: "/img.aaa.com/123/456.jpg"
func NewVHostPathRewriter(slashesCount int) PathRewriteFunc {
	return func(ctx *RequestCtx) []byte {
		path := stripLeadingSlashes(ctx.Path(), slashesCount)
		host := ctx.Host()
		if n := bytes.IndexByte(host, '/'); n >= 0 {
			host = nil
		}
		if len(host) == 0 {
			host = strInvalidHost
		}
		b := bytebufferpool.Get()
		b.B = append(b.B, '/')
		b.B = append(b.B, host...)
		b.B = append(b.B, path...)
		ctx.URI().SetPathBytes(b.B)
		bytebufferpool.Put(b)

		return ctx.Path()
	}
}

var strInvalidHost = []byte("invalid-host")

// NewPathSlashesStripper returns path rewriter, which strips slashesCount
// leading slashes from the path.
//
// Examples:
//
//   - slashesCount = 0, original path: "/foo/bar", result: "/foo/bar"
//   - slashesCount = 1, original path: "/foo/bar", result: "/bar"
//   - slashesCount = 2, original path: "/foo/bar", result: ""
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
//   - prefixSize = 0, original path: "/foo/bar", result: "/foo/bar"
//   - prefixSize = 3, original path: "/foo/bar", result: "o/bar"
//   - prefixSize = 7, original path: "/foo/bar", result: "r"
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
//
// It is prohibited copying FS values. Create new values instead.
type FS struct {
	noCopy noCopy //nolint:unused,structcheck

	// Path to the root directory to serve files from.
	Root string

	// AllowEmptyRoot controls what happens when Root is empty. When false (default) it will default to the
	// current working directory. An empty root is mostly useful when you want to use absolute paths
	// on windows that are on different filesystems. On linux setting your Root to "/" already allows you to use
	// absolute paths on any filesystem.
	AllowEmptyRoot bool

	// List of index file names to try opening during directory access.
	//
	// For example:
	//
	//     * index.html
	//     * index.htm
	//     * my-super-index.xml
	//
	// By default the list is empty.
	IndexNames []string

	// Index pages for directories without files matching IndexNames
	// are automatically generated if set.
	//
	// Directory index generation may be quite slow for directories
	// with many files (more than 1K), so it is discouraged enabling
	// index pages' generation for such directories.
	//
	// By default index pages aren't generated.
	GenerateIndexPages bool

	// Transparently compresses responses if set to true.
	//
	// The server tries minimizing CPU usage by caching compressed files.
	// It adds CompressedFileSuffix suffix to the original file name and
	// tries saving the resulting compressed file under the new file name.
	// So it is advisable to give the server write access to Root
	// and to all inner folders in order to minimize CPU usage when serving
	// compressed responses.
	//
	// Transparent compression is disabled by default.
	Compress bool

	// Uses brotli encoding and fallbacks to gzip in responses if set to true, uses gzip if set to false.
	//
	// This value has sense only if Compress is set.
	//
	// Brotli encoding is disabled by default.
	CompressBrotli bool

	// Path to the compressed root directory to serve files from. If this value
	// is empty, Root is used.
	CompressRoot string

	// Enables byte range requests if set to true.
	//
	// Byte range requests are disabled by default.
	AcceptByteRange bool

	// Path rewriting function.
	//
	// By default request path is not modified.
	PathRewrite PathRewriteFunc

	// PathNotFound fires when file is not found in filesystem
	// this functions tries to replace "Cannot open requested path"
	// server response giving to the programmer the control of server flow.
	//
	// By default PathNotFound returns
	// "Cannot open requested path"
	PathNotFound RequestHandler

	// Expiration duration for inactive file handlers.
	//
	// FSHandlerCacheDuration is used by default.
	CacheDuration time.Duration

	// Suffix to add to the name of cached compressed file.
	//
	// This value has sense only if Compress is set.
	//
	// FSCompressedFileSuffix is used by default.
	CompressedFileSuffix string

	// Suffixes list to add to compressedFileSuffix depending on encoding
	//
	// This value has sense only if Compress is set.
	//
	// FSCompressedFileSuffixes is used by default.
	CompressedFileSuffixes map[string]string

	// If CleanStop is set, the channel can be closed to stop the cleanup handlers
	// for the FS RequestHandlers created with NewRequestHandler.
	// NEVER close this channel while the handler is still being used!
	CleanStop chan struct{}

	once sync.Once
	h    RequestHandler
}

// FSCompressedFileSuffix is the suffix FS adds to the original file names
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
const FSCompressedFileSuffix = ".fasthttp.gz"

// FSCompressedFileSuffixes is the suffixes FS adds to the original file names depending on encoding
// when trying to store compressed file under the new file name.
// See FS.Compress for details.
var FSCompressedFileSuffixes = map[string]string{
	"gzip": ".fasthttp.gz",
	"br":   ".fasthttp.br",
}

// FSHandlerCacheDuration is the default expiration duration for inactive
// file handlers opened by FS.
const FSHandlerCacheDuration = 10 * time.Second

// FSHandler returns request handler serving static files from
// the given root folder.
//
// stripSlashes indicates how many leading slashes must be stripped
// from requested path before searching requested file in the root folder.
// Examples:
//
//   - stripSlashes = 0, original path: "/foo/bar", result: "/foo/bar"
//   - stripSlashes = 1, original path: "/foo/bar", result: "/bar"
//   - stripSlashes = 2, original path: "/foo/bar", result: ""
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
		IndexNames:         []string{"index.html"},
		GenerateIndexPages: true,
		AcceptByteRange:    true,
	}
	if stripSlashes > 0 {
		fs.PathRewrite = NewPathSlashesStripper(stripSlashes)
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
	fs.once.Do(fs.initRequestHandler)
	return fs.h
}

func (fs *FS) normalizeRoot(root string) string {
	// Serve files from the current working directory if Root is empty or if Root is a relative path.
	if (!fs.AllowEmptyRoot && len(root) == 0) || (len(root) > 0 && !filepath.IsAbs(root)) {
		path, err := os.Getwd()
		if err != nil {
			path = "."
		}
		root = path + "/" + root
	}
	// convert the root directory slashes to the native format
	root = filepath.FromSlash(root)

	// strip trailing slashes from the root path
	for len(root) > 0 && root[len(root)-1] == os.PathSeparator {
		root = root[:len(root)-1]
	}
	return root
}

func (fs *FS) initRequestHandler() {
	root := fs.normalizeRoot(fs.Root)

	compressRoot := fs.CompressRoot
	if len(compressRoot) == 0 {
		compressRoot = root
	} else {
		compressRoot = fs.normalizeRoot(compressRoot)
	}

	cacheDuration := fs.CacheDuration
	if cacheDuration <= 0 {
		cacheDuration = FSHandlerCacheDuration
	}

	compressedFileSuffixes := fs.CompressedFileSuffixes
	if len(compressedFileSuffixes["br"]) == 0 || len(compressedFileSuffixes["gzip"]) == 0 ||
		compressedFileSuffixes["br"] == compressedFileSuffixes["gzip"] {
		compressedFileSuffixes = FSCompressedFileSuffixes
	}

	if len(fs.CompressedFileSuffix) > 0 {
		compressedFileSuffixes["gzip"] = fs.CompressedFileSuffix
		compressedFileSuffixes["br"] = FSCompressedFileSuffixes["br"]
	}

	h := &fsHandler{
		root:                   root,
		indexNames:             fs.IndexNames,
		pathRewrite:            fs.PathRewrite,
		generateIndexPages:     fs.GenerateIndexPages,
		compress:               fs.Compress,
		compressBrotli:         fs.CompressBrotli,
		compressRoot:           compressRoot,
		pathNotFound:           fs.PathNotFound,
		acceptByteRange:        fs.AcceptByteRange,
		cacheDuration:          cacheDuration,
		compressedFileSuffixes: compressedFileSuffixes,
		cache:                  make(map[string]*fsFile),
		cacheBrotli:            make(map[string]*fsFile),
		cacheGzip:              make(map[string]*fsFile),
	}

	go func() {
		var pendingFiles []*fsFile

		clean := func() {
			pendingFiles = h.cleanCache(pendingFiles)
		}

		if fs.CleanStop != nil {
			t := time.NewTicker(cacheDuration / 2)
			for {
				select {
				case <-t.C:
					clean()
				case _, stillOpen := <-fs.CleanStop:
					// Ignore values send on the channel, only stop when it is closed.
					if !stillOpen {
						t.Stop()
						return
					}
				}
			}
		}
		for {
			time.Sleep(cacheDuration / 2)
			clean()
		}
	}()

	fs.h = h.handleRequest
}

type fsHandler struct {
	root                   string
	indexNames             []string
	pathRewrite            PathRewriteFunc
	pathNotFound           RequestHandler
	generateIndexPages     bool
	compress               bool
	compressBrotli         bool
	compressRoot           string
	acceptByteRange        bool
	cacheDuration          time.Duration
	compressedFileSuffixes map[string]string

	cache       map[string]*fsFile
	cacheBrotli map[string]*fsFile
	cacheGzip   map[string]*fsFile
	cacheLock   sync.Mutex

	smallFileReaderPool  sync.Pool
	smallRangeReaderPool sync.Pool
}

type fsFile struct {
	h             *fsHandler
	f             *os.File
	dirIndex      []byte
	contentType   string
	contentLength int
	compressed    bool

	lastModified    time.Time
	lastModifiedStr []byte

	t            time.Time
	readersCount int

	bigFiles     []*bigFileReader
	bigFilesLock sync.Mutex

	rangeBigFiles     []*bigRangeReader
	rangeBigFilesLock sync.Mutex
}

func (ff *fsFile) NewReader() (io.Reader, error) {
	if ff.isBig() {
		r, err := ff.bigFileReader()
		if err != nil {
			ff.decReadersCount()
		}
		return r, err
	}
	return ff.smallFileReader()
}

func (ff *fsFile) smallFileReader() (io.Reader, error) {
	v := ff.h.smallFileReaderPool.Get()
	if v == nil {
		v = &fsSmallFileReader{}
	}
	r := v.(*fsSmallFileReader)
	r.ff = ff
	if r.offset > 0 {
		panic("BUG: fsSmallFileReader with non-nil offset found in the pool")
	}
	return r, nil
}

// files bigger than this size are sent with sendfile
const maxSmallFileSize = 2 * 4096

func (ff *fsFile) isBig() bool {
	return ff.contentLength > maxSmallFileSize && len(ff.dirIndex) == 0
}

func (ff *fsFile) bigFileReader() (io.Reader, error) {
	if ff.f == nil {
		return nil, errors.New("bug: ff.f must be non-nil in bigFileReader")
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
		return nil, fmt.Errorf("cannot open already opened file: %w", err)
	}
	return &bigFileReader{
		f:  f,
		ff: ff,
	}, nil
}

func (ff *fsFile) Release() {
	if ff.f != nil {
		_ = ff.f.Close()

		if ff.isBig() {
			ff.bigFilesLock.Lock()
			for _, r := range ff.bigFiles {
				_ = r.f.Close()
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

func (ff *fsFile) NewRangeReader() (io.Reader, error) {
	if ff.isBig() {
		r, err := ff.bigRangeReader()
		if err != nil {
			ff.decReadersCount()
		}
		return r, err
	}
	return ff.smallRangeReader(), nil
}

func (ff *fsFile) smallRangeReader() io.Reader {
	v := ff.h.smallRangeReaderPool.Get()
	if v == nil {
		v = &smallRangeReader{
			sta: make([]int, 64),
			end: make([]int, 64),
			buf: make([]byte, 4096),
		}
	}
	r := v.(*smallRangeReader)
	r.ff = ff
	r.sta, r.end, r.buf = r.sta[:0], r.end[:0], r.buf[:0]
	r.hf = true
	r.he = false
	r.hasCurRangeBodyHeader = false
	r.cRange = 0
	r.boundary = randomBoundary()
	return r
}

func (ff *fsFile) bigRangeReader() (io.Reader, error) {
	if ff.f == nil {
		panic("BUG: ff.f must be non-nil in bigRangeReader")
	}
	var r io.Reader

	ff.rangeBigFilesLock.Lock()
	n := len(ff.rangeBigFiles)
	if n > 0 {
		r = ff.rangeBigFiles[n-1]
		ff.rangeBigFiles = ff.rangeBigFiles[:n-1]
	}
	ff.rangeBigFilesLock.Unlock()

	if r != nil {
		return r, nil
	}
	f, err := os.Open(ff.f.Name())
	if err != nil {
		return nil, fmt.Errorf("cannot open already opened file: %s", err)
	}
	rr := &bigRangeReader{
		ff:                    ff,
		f:                     f,
		sta:                   make([]int, 64),
		end:                   make([]int, 64),
		buf:                   make([]byte, 4096),
		hf:                    true,
		he:                    false,
		hasCurRangeBodyHeader: false,
		cRange:                0,
		boundary:              randomBoundary(),
	}
	rr.sta, rr.end, rr.buf = rr.sta[:0], rr.end[:0], rr.buf[:0]
	return rr, nil
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
		// fast path. Send file must be triggered
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
}

func (r *fsSmallFileReader) Close() error {
	ff := r.ff
	ff.decReadersCount()
	r.ff = nil
	r.offset = 0
	ff.h.smallFileReaderPool.Put(r)
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
		panic("BUG: no-zero offset! Read() mustn't be called before WriteTo()")
	}
	ff := r.ff

	var n int
	var err error
	if ff.f == nil {
		n, err = w.Write(ff.dirIndex)
		return int64(n), err
	}
	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	bufv := copyBufPool.Get()
	buf := bufv.([]byte)
	for err == nil {
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

func (h *fsHandler) cleanCache(pendingFiles []*fsFile) []*fsFile {
	var filesToRelease []*fsFile

	h.cacheLock.Lock()

	// Close files which couldn't be closed before due to non-zero
	// readers count on the previous run.
	var remainingFiles []*fsFile
	for _, ff := range pendingFiles {
		if ff.readersCount > 0 {
			remainingFiles = append(remainingFiles, ff)
		} else {
			filesToRelease = append(filesToRelease, ff)
		}
	}
	pendingFiles = remainingFiles

	pendingFiles, filesToRelease = cleanCacheNolock(h.cache, pendingFiles, filesToRelease, h.cacheDuration)
	pendingFiles, filesToRelease = cleanCacheNolock(h.cacheBrotli, pendingFiles, filesToRelease, h.cacheDuration)
	pendingFiles, filesToRelease = cleanCacheNolock(h.cacheGzip, pendingFiles, filesToRelease, h.cacheDuration)

	h.cacheLock.Unlock()

	for _, ff := range filesToRelease {
		ff.Release()
	}

	return pendingFiles
}

func cleanCacheNolock(cache map[string]*fsFile, pendingFiles, filesToRelease []*fsFile, cacheDuration time.Duration) ([]*fsFile, []*fsFile) {
	t := time.Now()
	for k, ff := range cache {
		if t.Sub(ff.t) > cacheDuration {
			if ff.readersCount > 0 {
				// There are pending readers on stale file handle,
				// so we cannot close it. Put it into pendingFiles
				// so it will be closed later.
				pendingFiles = append(pendingFiles, ff)
			} else {
				filesToRelease = append(filesToRelease, ff)
			}
			delete(cache, k)
		}
	}
	return pendingFiles, filesToRelease
}

func (h *fsHandler) pathToFilePath(path string) string {
	return filepath.FromSlash(h.root + path)
}

func (h *fsHandler) filePathToCompressed(filePath string) string {
	if h.root == h.compressRoot {
		return filePath
	}
	if !strings.HasPrefix(filePath, h.root) {
		return filePath
	}
	return filepath.FromSlash(h.compressRoot + filePath[len(h.root):])
}

func (h *fsHandler) handleRequest(ctx *RequestCtx) {
	var path []byte
	if h.pathRewrite != nil {
		path = h.pathRewrite(ctx)
	} else {
		path = ctx.Path()
	}
	hasTrailingSlash := len(path) > 0 && path[len(path)-1] == '/'
	path = stripTrailingSlashes(path)

	if n := bytes.IndexByte(path, 0); n >= 0 {
		ctx.Logger().Printf("cannot serve path with nil byte at position %d: %q", n, path)
		ctx.Error("Are you a hacker?", StatusBadRequest)
		return
	}
	if h.pathRewrite != nil {
		// There is no need to check for '/../' if path = ctx.Path(),
		// since ctx.Path must normalize and sanitize the path.

		if n := bytes.Index(path, strSlashDotDotSlash); n >= 0 {
			ctx.Logger().Printf("cannot serve path with '/../' at position %d due to security reasons: %q", n, path)
			ctx.Error("Internal Server Error", StatusInternalServerError)
			return
		}
	}

	mustCompress := false
	fileCache := h.cache
	fileEncoding := ""
	byteRange := ctx.Request.Header.peek(strRange)
	if len(byteRange) == 0 && h.compress {
		if h.compressBrotli && ctx.Request.Header.HasAcceptEncodingBytes(strBr) {
			mustCompress = true
			fileCache = h.cacheBrotli
			fileEncoding = "br"
		} else if ctx.Request.Header.HasAcceptEncodingBytes(strGzip) {
			mustCompress = true
			fileCache = h.cacheGzip
			fileEncoding = "gzip"
		}
	}

	h.cacheLock.Lock()
	ff, ok := fileCache[string(path)]
	if ok {
		ff.readersCount++
	}
	h.cacheLock.Unlock()

	if !ok {
		pathStr := string(path)
		filePath := h.pathToFilePath(pathStr)

		var err error
		ff, err = h.openFSFile(filePath, mustCompress, fileEncoding)
		if mustCompress && err == errNoCreatePermission {
			ctx.Logger().Printf("insufficient permissions for saving compressed file for %q. Serving uncompressed file. "+
				"Allow write access to the directory with this file in order to improve fasthttp performance", filePath)
			mustCompress = false
			ff, err = h.openFSFile(filePath, mustCompress, fileEncoding)
		}
		if err == errDirIndexRequired {
			if !hasTrailingSlash {
				ctx.RedirectBytes(append(path, '/'), StatusFound)
				return
			}
			ff, err = h.openIndexFile(ctx, filePath, mustCompress, fileEncoding)
			if err != nil {
				ctx.Logger().Printf("cannot open dir index %q: %v", filePath, err)
				ctx.Error("Directory index is forbidden", StatusForbidden)
				return
			}
		} else if err != nil {
			ctx.Logger().Printf("cannot open file %q: %v", filePath, err)
			if h.pathNotFound == nil {
				ctx.Error("Cannot open requested path", StatusNotFound)
			} else {
				ctx.SetStatusCode(StatusNotFound)
				h.pathNotFound(ctx)
			}
			return
		}

		h.cacheLock.Lock()
		ff1, ok := fileCache[pathStr]
		if !ok {
			fileCache[pathStr] = ff
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

	var r io.Reader
	var err error
	if h.acceptByteRange && len(byteRange) > 0 {
		r, err = ff.NewRangeReader()
	} else {
		r, err = ff.NewReader()
	}

	if err != nil {
		ctx.Logger().Printf("cannot obtain file reader for path=%q: %v", path, err)
		ctx.Error("Internal Server Error", StatusInternalServerError)
		return
	}

	hdr := &ctx.Response.Header
	if ff.compressed {
		if fileEncoding == "br" {
			hdr.SetContentEncodingBytes(strBr)
		} else if fileEncoding == "gzip" {
			hdr.SetContentEncodingBytes(strGzip)
		}
	}

	statusCode := StatusOK
	contentLength := ff.contentLength
	if h.acceptByteRange {
		hdr.setNonSpecial(strAcceptRanges, strBytes)
		if len(byteRange) > 0 {
			staList, endList, err := ParseByteRanges(byteRange, contentLength)
			if err != nil {
				r.(io.Closer).Close()
				ctx.Logger().Printf("Cannot parse byte range %q for path=%q,error=%s", byteRange, path, err)
				ctx.Error("Range Not Satisfiable", StatusRequestedRangeNotSatisfiable)
				return
			}

			if err = r.(byteRangeHandler).ByteRangeUpdate(staList, endList); err != nil {
				r.(io.Closer).Close()
				ctx.Logger().Printf("Cannot seek byte range %q for path=%q, error=%s", byteRange, path, err)
				ctx.Error("Internal Server Error", StatusInternalServerError)
				return
			}
			switch {
			case len(staList) == 1:
				hdr.SetContentRange(staList[0], endList[0], contentLength)
			case len(staList) > 1:
				hdr.SetContentType(fmt.Sprintf("multipart/byteranges; boundary=%s", r.(byteRangeHandler).Boundary()))
			}
			contentLength = r.(byteRangeHandler).ByteRangeLength()
			statusCode = StatusPartialContent
		}
	}

	hdr.setNonSpecial(strLastModified, ff.lastModifiedStr)
	if !ctx.IsHead() {
		ctx.SetBodyStream(r, contentLength)
	} else {
		ctx.Response.ResetBody()
		ctx.Response.SkipBody = true
		ctx.Response.Header.SetContentLength(contentLength)
		if rc, ok := r.(io.Closer); ok {
			if err := rc.Close(); err != nil {
				ctx.Logger().Printf("cannot close file reader: %v", err)
				ctx.Error("Internal Server Error", StatusInternalServerError)
				return
			}
		}
	}
	hdr.noDefaultContentType = true
	if len(hdr.ContentType()) == 0 {
		ctx.SetContentType(ff.contentType)
	}
	ctx.SetStatusCode(statusCode)
}

// ParseByteRange parses 'Range: bytes=...' header value.
//
// It follows https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.35 .
func ParseByteRanges(byteRange []byte, contentLength int) (startPos, endPos []int, err error) {
	b := byteRange
	if !bytes.HasPrefix(b, strBytes) {
		return startPos, endPos, fmt.Errorf("unsupported range units: %q. Expecting %q", byteRange, strBytes)
	}
	b = b[len(strBytes):]
	if len(b) == 0 || b[0] != '=' {
		return startPos, endPos, fmt.Errorf("missing byte range in %q", byteRange)
	}
	b = b[1:]

	var n, sta, end, v int

	for _, ra := range bytes.Split(b, s2b(",")) {
		n = bytes.IndexByte(ra, '-')
		if n < 0 {
			return startPos, endPos, fmt.Errorf("missing the end position of byte range in %q", byteRange)
		}
		if n == 0 {
			v, err = ParseUint(ra[n+1:])
			if err != nil {
				return startPos, endPos, err
			}
			sta = contentLength - v
			if sta < 0 {
				sta = 0
			}
			startPos = append(startPos, sta)
			endPos = append(endPos, contentLength-1)
			continue
		}
		if sta, err = ParseUint(ra[:n]); err != nil {
			return startPos, endPos, err
		}
		if sta >= contentLength {
			return startPos, endPos, fmt.Errorf("the start position of byte range cannot exceed %d. byte range %q", contentLength-1, byteRange)
		}
		ra = ra[n+1:]
		if len(ra) == 0 {
			startPos = append(startPos, sta)
			endPos = append(endPos, contentLength-1)
			continue
		}
		if end, err = ParseUint(ra); err != nil {
			return startPos, endPos, err
		}
		if end >= contentLength {
			end = contentLength - 1
		}
		if end < sta {
			return startPos, endPos, fmt.Errorf("the start position of byte range cannot exceed the end position. byte range %q", byteRange)
		}
		startPos = append(startPos, sta)
		endPos = append(endPos, end)
	}
	return startPos, endPos, nil
}

func (h *fsHandler) openIndexFile(ctx *RequestCtx, dirPath string, mustCompress bool, fileEncoding string) (*fsFile, error) {
	for _, indexName := range h.indexNames {
		indexFilePath := dirPath + "/" + indexName
		ff, err := h.openFSFile(indexFilePath, mustCompress, fileEncoding)
		if err == nil {
			return ff, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot open file %q: %w", indexFilePath, err)
		}
	}

	if !h.generateIndexPages {
		return nil, fmt.Errorf("cannot access directory without index page. Directory %q", dirPath)
	}

	return h.createDirIndex(ctx.URI(), dirPath, mustCompress, fileEncoding)
}

var (
	errDirIndexRequired   = errors.New("directory index required")
	errNoCreatePermission = errors.New("no 'create file' permissions")
)

func (h *fsHandler) createDirIndex(base *URI, dirPath string, mustCompress bool, fileEncoding string) (*fsFile, error) {
	w := &bytebufferpool.ByteBuffer{}

	basePathEscaped := html.EscapeString(string(base.Path()))
	_, _ = fmt.Fprintf(w, "<html><head><title>%s</title><style>.dir { font-weight: bold }</style></head><body>", basePathEscaped)
	_, _ = fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	_, _ = fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		var parentURI URI
		base.CopyTo(&parentURI)
		parentURI.Update(string(base.Path()) + "/..")
		parentPathEscaped := html.EscapeString(string(parentURI.Path()))
		_, _ = fmt.Fprintf(w, `<li><a href="%s" class="dir">..</a></li>`, parentPathEscaped)
	}

	f, err := os.Open(dirPath)
	if err != nil {
		return nil, err
	}

	fileinfos, err := f.Readdir(0)
	_ = f.Close()
	if err != nil {
		return nil, err
	}

	fm := make(map[string]os.FileInfo, len(fileinfos))
	filenames := make([]string, 0, len(fileinfos))
nestedContinue:
	for _, fi := range fileinfos {
		name := fi.Name()
		for _, cfs := range h.compressedFileSuffixes {
			if strings.HasSuffix(name, cfs) {
				// Do not show compressed files on index page.
				continue nestedContinue
			}
		}
		fm[name] = fi
		filenames = append(filenames, name)
	}

	var u URI
	base.CopyTo(&u)
	u.Update(string(u.Path()) + "/")

	sort.Strings(filenames)
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
		_, _ = fmt.Fprintf(w, `<li><a href="%s" class="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, className, html.EscapeString(name), auxStr, fsModTime(fi.ModTime()))
	}

	_, _ = fmt.Fprintf(w, "</ul></body></html>")

	if mustCompress {
		var zbuf bytebufferpool.ByteBuffer
		if fileEncoding == "br" {
			zbuf.B = AppendBrotliBytesLevel(zbuf.B, w.B, CompressDefaultCompression)
		} else if fileEncoding == "gzip" {
			zbuf.B = AppendGzipBytesLevel(zbuf.B, w.B, CompressDefaultCompression)
		}
		w = &zbuf
	}

	dirIndex := w.B
	lastModified := time.Now()
	ff := &fsFile{
		h:               h,
		dirIndex:        dirIndex,
		contentType:     "text/html; charset=utf-8",
		contentLength:   len(dirIndex),
		compressed:      mustCompress,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),

		t: lastModified,
	}
	return ff, nil
}

const (
	fsMinCompressRatio        = 0.8
	fsMaxCompressibleFileSize = 8 * 1024 * 1024
)

func (h *fsHandler) compressAndOpenFSFile(filePath string, fileEncoding string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
	}

	if fileInfo.IsDir() {
		_ = f.Close()
		return nil, errDirIndexRequired
	}

	if strings.HasSuffix(filePath, h.compressedFileSuffixes[fileEncoding]) ||
		fileInfo.Size() > fsMaxCompressibleFileSize ||
		!isFileCompressible(f, fsMinCompressRatio) {
		return h.newFSFile(f, fileInfo, false, "")
	}

	compressedFilePath := h.filePathToCompressed(filePath)
	if compressedFilePath != filePath {
		if err := os.MkdirAll(filepath.Dir(compressedFilePath), os.ModePerm); err != nil {
			return nil, err
		}
	}
	compressedFilePath += h.compressedFileSuffixes[fileEncoding]

	absPath, err := filepath.Abs(compressedFilePath)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot determine absolute path for %q: %v", compressedFilePath, err)
	}

	flock := getFileLock(absPath)
	flock.Lock()
	ff, err := h.compressFileNolock(f, fileInfo, filePath, compressedFilePath, fileEncoding)
	flock.Unlock()

	return ff, err
}

func (h *fsHandler) compressFileNolock(f *os.File, fileInfo os.FileInfo, filePath, compressedFilePath string, fileEncoding string) (*fsFile, error) {
	// Attempt to open compressed file created by another concurrent
	// goroutine.
	// It is safe opening such a file, since the file creation
	// is guarded by file mutex - see getFileLock call.
	if _, err := os.Stat(compressedFilePath); err == nil {
		_ = f.Close()
		return h.newCompressedFSFile(compressedFilePath, fileEncoding)
	}

	// Create temporary file, so concurrent goroutines don't use
	// it until it is created.
	tmpFilePath := compressedFilePath + ".tmp"
	zf, err := os.Create(tmpFilePath)
	if err != nil {
		_ = f.Close()
		if !os.IsPermission(err) {
			return nil, fmt.Errorf("cannot create temporary file %q: %w", tmpFilePath, err)
		}
		return nil, errNoCreatePermission
	}
	if fileEncoding == "br" {
		zw := acquireStacklessBrotliWriter(zf, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if err1 := zw.Flush(); err == nil {
			err = err1
		}
		releaseStacklessBrotliWriter(zw, CompressDefaultCompression)
	} else if fileEncoding == "gzip" {
		zw := acquireStacklessGzipWriter(zf, CompressDefaultCompression)
		_, err = copyZeroAlloc(zw, f)
		if err1 := zw.Flush(); err == nil {
			err = err1
		}
		releaseStacklessGzipWriter(zw, CompressDefaultCompression)
	}
	_ = zf.Close()
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("error when compressing file %q to %q: %w", filePath, tmpFilePath, err)
	}
	if err = os.Chtimes(tmpFilePath, time.Now(), fileInfo.ModTime()); err != nil {
		return nil, fmt.Errorf("cannot change modification time to %v for tmp file %q: %v",
			fileInfo.ModTime(), tmpFilePath, err)
	}
	if err = os.Rename(tmpFilePath, compressedFilePath); err != nil {
		return nil, fmt.Errorf("cannot move compressed file from %q to %q: %w", tmpFilePath, compressedFilePath, err)
	}
	return h.newCompressedFSFile(compressedFilePath, fileEncoding)
}

func (h *fsHandler) newCompressedFSFile(filePath string, fileEncoding string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot open compressed file %q: %w", filePath, err)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot obtain info for compressed file %q: %w", filePath, err)
	}
	return h.newFSFile(f, fileInfo, true, fileEncoding)
}

func (h *fsHandler) openFSFile(filePath string, mustCompress bool, fileEncoding string) (*fsFile, error) {
	filePathOriginal := filePath
	if mustCompress {
		filePath += h.compressedFileSuffixes[fileEncoding]
	}

	f, err := os.Open(filePath)
	if err != nil {
		if mustCompress && os.IsNotExist(err) {
			return h.compressAndOpenFSFile(filePathOriginal, fileEncoding)
		}
		return nil, err
	}

	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cannot obtain info for file %q: %w", filePath, err)
	}

	if fileInfo.IsDir() {
		_ = f.Close()
		if mustCompress {
			return nil, fmt.Errorf("directory with unexpected suffix found: %q. Suffix: %q",
				filePath, h.compressedFileSuffixes[fileEncoding])
		}
		return nil, errDirIndexRequired
	}

	if mustCompress {
		fileInfoOriginal, err := os.Stat(filePathOriginal)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cannot obtain info for original file %q: %w", filePathOriginal, err)
		}

		// Only re-create the compressed file if there was more than a second between the mod times.
		// On MacOS the gzip seems to truncate the nanoseconds in the mod time causing the original file
		// to look newer than the gzipped file.
		if fileInfoOriginal.ModTime().Sub(fileInfo.ModTime()) >= time.Second {
			// The compressed file became stale. Re-create it.
			_ = f.Close()
			_ = os.Remove(filePath)
			return h.compressAndOpenFSFile(filePathOriginal, fileEncoding)
		}
	}

	return h.newFSFile(f, fileInfo, mustCompress, fileEncoding)
}

func (h *fsHandler) newFSFile(f *os.File, fileInfo os.FileInfo, compressed bool, fileEncoding string) (*fsFile, error) {
	n := fileInfo.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		_ = f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	// detect content-type
	ext := fileExtension(fileInfo.Name(), compressed, h.compressedFileSuffixes[fileEncoding])
	contentType := mime.TypeByExtension(ext)
	if len(contentType) == 0 {
		data, err := readFileHeader(f, compressed, fileEncoding)
		if err != nil {
			return nil, fmt.Errorf("cannot read header of the file %q: %w", f.Name(), err)
		}
		contentType = http.DetectContentType(data)
	}

	lastModified := fileInfo.ModTime()
	ff := &fsFile{
		h:               h,
		f:               f,
		contentType:     contentType,
		contentLength:   contentLength,
		compressed:      compressed,
		lastModified:    lastModified,
		lastModifiedStr: AppendHTTPDate(nil, lastModified),

		t: time.Now(),
	}
	return ff, nil
}

func readFileHeader(f *os.File, compressed bool, fileEncoding string) ([]byte, error) {
	r := io.Reader(f)
	var (
		br *brotli.Reader
		zr *gzip.Reader
	)
	if compressed {
		var err error
		if fileEncoding == "br" {
			if br, err = acquireBrotliReader(f); err != nil {
				return nil, err
			}
			r = br
		} else if fileEncoding == "gzip" {
			if zr, err = acquireGzipReader(f); err != nil {
				return nil, err
			}
			r = zr
		}
	}

	lr := &io.LimitedReader{
		R: r,
		N: 512,
	}
	data, err := io.ReadAll(lr)
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	if br != nil {
		releaseBrotliReader(br)
	}

	if zr != nil {
		releaseGzipReader(zr)
	}

	return data, err
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

func fileExtension(path string, compressed bool, compressedFileSuffix string) string {
	if compressed && strings.HasSuffix(path, compressedFileSuffix) {
		path = path[:len(path)-len(compressedFileSuffix)]
	}
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}

// FileLastModified returns last modified time for the file.
func FileLastModified(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return zeroTime, err
	}
	fileInfo, err := f.Stat()
	_ = f.Close()
	if err != nil {
		return zeroTime, err
	}
	return fsModTime(fileInfo.ModTime()), nil
}

func fsModTime(t time.Time) time.Time {
	return t.In(time.UTC).Truncate(time.Second)
}

var filesLockMap sync.Map

func getFileLock(absPath string) *sync.Mutex {
	v, _ := filesLockMap.LoadOrStore(absPath, &sync.Mutex{})
	filelock := v.(*sync.Mutex)
	return filelock
}

type byteRangeHandler interface {
	ByteRangeUpdate(sta, end []int) error
	ByteRangeLength() int
	Boundary() string
	IsMultiRange() bool
}

type smallRangeReader struct {
	ff  *fsFile
	sta []int
	end []int

	// handle multi range
	buf                   []byte
	hf                    bool
	he                    bool
	hasCurRangeBodyHeader bool
	cRange                int
	boundary              string
}

func (r *smallRangeReader) ByteRangeUpdate(sta, end []int) error {
	for i := range sta {
		r.sta = append(r.sta, sta[i])
		r.end = append(r.end, end[i]+1)
	}
	return nil
}

func (r *smallRangeReader) IsMultiRange() bool {
	return len(r.sta) > 1
}

func (r *smallRangeReader) Boundary() string {
	return r.boundary
}

func (r *smallRangeReader) ByteRangeLength() int {
	if !r.IsMultiRange() {
		return r.end[0] - r.sta[0]
	}
	return multiRangeLength(r.sta, r.end, r.ff.contentLength, r.ff.contentType, r.boundary)
}

func (r *smallRangeReader) Close() error {
	ff := r.ff
	ff.decReadersCount()
	r.ff = nil

	r.sta, r.end, r.buf = r.sta[:0], r.end[:0], r.buf[:0]
	r.cRange = 0
	r.hf = true
	r.he = false
	r.hasCurRangeBodyHeader = false
	r.boundary = ""
	ff.h.smallRangeReaderPool.Put(r)
	return nil
}

func (r *smallRangeReader) Read(p []byte) (int, error) {
	ff := r.ff
	var err error
	cPos, cLen, n := 0, 0, 0

	if r.cRange >= len(r.sta) && len(r.buf) == 0 && r.he {
		return 0, io.EOF
	}

	if r.IsMultiRange() {
		cLen = len(p[cPos:])
		n = copy(p[cPos:], r.buf)
		if len(r.buf) > cLen {
			r.buf = r.buf[n:]
			return n, nil
		}
		cPos += n
		r.buf = r.buf[:0]
	}

	for i := r.cRange; i < len(r.sta); i++ {
		if r.sta[i] >= r.end[i] {
			continue
		}

		if r.IsMultiRange() && !r.hasCurRangeBodyHeader {
			r.hasCurRangeBodyHeader = true
			multiRangeBodyHeader(&r.buf, r.sta[i], r.end[i], r.ff.contentLength, r.ff.contentType, r.boundary, r.hf)
			r.hf = false
			cLen = len(p[cPos:])
			n = copy(p[cPos:], r.buf)
			cPos += n
			if len(r.buf) > cLen {
				r.buf = r.buf[n:]
				return cPos, nil
			}
			r.buf = r.buf[:0]
		}

		cLen = len(p[cPos:])

		// handle file
		if ff.f != nil {
			if r.end[i]-r.sta[i] > cLen {
				n, err = ff.f.ReadAt(p[cPos:], int64(r.sta[i]))
				r.sta[i] += n
				return cPos + n, err
			}

			// todo use pool
			cBody := make([]byte, r.end[i]-r.sta[i])
			n, err = ff.f.ReadAt(cBody, int64(r.sta[i]))
			if err != nil && err != io.EOF {
				return cPos + n, err
			}
			n = copy(p[cPos:], cBody)
		} else {
			// handle dir
			n = copy(p[cPos:], ff.dirIndex[r.sta[i]:r.end[i]])
			if r.end[i]-r.sta[i] > cLen {
				r.sta[i] += n
				return cPos + n, nil
			}
		}

		cPos += n
		r.cRange = i + 1
		r.hasCurRangeBodyHeader = false
	}

	if r.IsMultiRange() && !r.he {
		multiRangeBodyEnd(&r.buf, r.boundary)
		r.he = true
		n = copy(p[cPos:], r.buf)
		if len(r.buf) > len(p[cPos:]) {
			r.buf = r.buf[n:]
			return cPos + n, nil
		}
		cPos += n
		r.buf = r.buf[:0]
	}
	return cPos, io.EOF
}

func (r *smallRangeReader) WriteTo(w io.Writer) (int64, error) {
	ff := r.ff
	var err error
	cPos, cLen, n, sum := 0, 0, 0, 0
	bufv := copyBufPool.Get()
	buf := bufv.([]byte)
	hf := true
	if ff.f == nil {
		for i := range r.sta {

			if r.IsMultiRange() {
				buf = buf[:0]
				if i > 0 {
					hf = false
				}
				multiRangeBodyHeader(&buf, r.sta[i], r.end[i], ff.contentLength, ff.contentType, r.boundary, hf)
				nw, errw := w.Write(buf)
				if errw == nil && nw != len(buf) {
					panic("BUG: buf returned(n, nil),where n != len(buf)")
				}
				sum += nw
				if errw != nil {
					return int64(sum), errw
				}
			}

			n, err = w.Write(ff.dirIndex[r.sta[i]:r.end[i]])
			sum += n
			if err != nil {
				return int64(sum), err
			}
		}
		if r.IsMultiRange() {
			buf = buf[:0]
			multiRangeBodyEnd(&buf, r.boundary)
			nw, errw := w.Write(buf)
			if errw == nil && nw != len(buf) {
				panic("BUG: buf returned (n, nil), where n != len(buf)")
			}
			sum += nw
			err = errw
		}
		return int64(sum), err
	}

	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	for i := range r.sta {

		if r.IsMultiRange() {
			buf = buf[:0]
			if i > 0 {
				hf = false
			}
			multiRangeBodyHeader(&buf, r.sta[i], r.end[i], ff.contentLength, ff.contentType, r.boundary, hf)
			nw, errw := w.Write(buf)
			if errw == nil && nw != len(buf) {
				panic("BUG: buf returned(n, nil),where n != len(buf)")
			}
			sum += nw
			if errw != nil {
				return int64(sum), errw
			}
		}

		cPos = r.sta[i]
		buf = buf[:4096]
		for err == nil {
			cLen = r.end[i] - cPos
			if cLen <= 0 {
				break
			}
			if len(buf) > cLen {
				buf = buf[:cLen]
			}
			n, err = ff.f.ReadAt(buf, int64(cPos))
			nw, errw := w.Write(buf[:n])
			cPos += nw
			sum += nw
			if errw == nil && nw != n {
				panic("BUG: Write(p) returned (n, nil), where n != len(p)")
			}
			if err == nil {
				err = errw
			}
		}
	}
	if err == io.EOF {
		err = nil
	}
	if r.IsMultiRange() {
		buf = buf[:0]
		multiRangeBodyEnd(&buf, r.boundary)
		nw, errw := w.Write(buf)
		if errw == nil && nw != len(buf) {
			panic("BUG: buf returned (n, nil), where n != len(buf)")
		}
		sum += nw
		if err == nil {
			err = errw
		}
	}
	return int64(sum), err
}

type bigRangeReader struct {
	ff  *fsFile
	f   *os.File
	sta []int
	end []int

	// handle multi range
	buf                   []byte
	hf                    bool
	he                    bool
	hasCurRangeBodyHeader bool
	cRange                int
	boundary              string
}

func (r *bigRangeReader) ByteRangeUpdate(sta, end []int) error {
	for i := range sta {
		r.sta = append(r.sta, sta[i])
		r.end = append(r.end, end[i]+1)
	}
	return nil
}

func (r *bigRangeReader) IsMultiRange() bool {
	return len(r.sta) > 1
}

func (r *bigRangeReader) Boundary() string {
	return r.boundary
}

func (r *bigRangeReader) ByteRangeLength() int {
	if !r.IsMultiRange() {
		return r.end[0] - r.sta[0]
	}
	return multiRangeLength(r.sta, r.end, r.ff.contentLength, r.ff.contentType, r.boundary)
}

func (r *bigRangeReader) Close() error {
	n, err := r.f.Seek(0, 0)
	if err == nil {
		if n != 0 {
			panic("BUG: File.Seek(0,0) returned (non-zero, nil)")
		}
		ff := r.ff
		ff.rangeBigFilesLock.Lock()
		r.end, r.sta, r.buf = r.end[:0], r.sta[:0], r.buf[:0]
		r.cRange = 0
		r.hf = true
		r.he = false
		r.hasCurRangeBodyHeader = false

		ff.rangeBigFiles = append(ff.rangeBigFiles, r)
		ff.rangeBigFilesLock.Unlock()
	} else {
		r.f.Close()
	}
	r.ff.decReadersCount()
	return err
}

func (r *bigRangeReader) Read(p []byte) (int, error) {
	if r.cRange >= len(r.sta) && len(r.buf) == 0 && r.he {
		return 0, io.EOF
	}

	ff := r.ff
	var err error
	cPos, cLen, n := 0, 0, 0

	if r.IsMultiRange() {
		cLen = len(p[cPos:])
		n = copy(p[cPos:], r.buf)
		if len(r.buf) > cLen {
			r.buf = r.buf[n:]
			return n, nil
		}
		cPos += n
		r.buf = r.buf[:0]
	}

	for i := r.cRange; i < len(r.sta); i++ {
		if r.sta[i] >= r.end[i] {
			continue
		}

		if r.IsMultiRange() && !r.hasCurRangeBodyHeader {
			r.hasCurRangeBodyHeader = true
			multiRangeBodyHeader(&r.buf, r.sta[i], r.end[i], r.ff.contentLength, r.ff.contentType, r.boundary, r.hf)
			r.hf = false
			cLen = len(p[cPos:])
			n = copy(p[cPos:], r.buf)
			cPos += n
			if len(r.buf) > cLen {
				r.buf = r.buf[n:]
				return cPos, nil
			}
			r.buf = r.buf[:0]
		}

		cLen = len(p[cPos:])
		if r.end[i]-r.sta[i] > cLen {
			n, err = r.f.ReadAt(p[cPos:], int64(r.sta[i]))
			r.sta[i] += n
			return cPos + n, err
		}

		// todo use pool
		cBody := make([]byte, r.end[i]-r.sta[i])
		n, err = ff.f.ReadAt(cBody, int64(r.sta[i]))
		if err != nil && err != io.EOF {
			return cPos + n, err
		}
		n = copy(p[cPos:], cBody)
		cPos += n
		r.cRange = i + 1
		r.hasCurRangeBodyHeader = false
	}

	if r.IsMultiRange() && !r.he {
		multiRangeBodyEnd(&r.buf, r.boundary)
		r.he = true
		n = copy(p[cPos:], r.buf)
		if len(r.buf) > len(p[cPos:]) {
			r.buf = r.buf[n:]
			return cPos + n, nil
		}
		cPos += n
		r.buf = r.buf[:0]
	}
	return cPos, io.EOF
}

func (r *bigRangeReader) WriteTo(w io.Writer) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	var err error
	cPos, cLen, n, sum := 0, 0, 0, 0
	hf := true

	bufv := copyBufPool.Get()
	buf := bufv.([]byte)

	for i := range r.sta {

		if r.IsMultiRange() {
			buf = buf[:0]
			if i > 0 {
				hf = false
			}
			multiRangeBodyHeader(&buf, r.sta[i], r.end[i], r.ff.contentLength, r.ff.contentType, r.boundary, hf)
			nw, errw := w.Write(buf)
			if errw == nil && nw != len(buf) {
				panic("BUG: buf returned(n, nil),where n != len(buf)")
			}
			sum += nw
			if errw != nil {
				return int64(sum), errw
			}
		}

		cPos = r.sta[i]
		buf = buf[:4096]
		for err == nil {
			cLen = r.end[i] - cPos
			if cLen <= 0 {
				break
			}
			if len(buf) > cLen {
				buf = buf[:cLen]
			}
			n, err = r.f.ReadAt(buf, int64(cPos))
			nw, errw := w.Write(buf[:n])
			cPos += nw
			sum += nw
			if errw == nil && nw != n {
				panic("BUG: Write(p) returned (n, nil), where n != len(p)")
			}
			if err == nil {
				err = errw
			}
		}
	}
	if err == io.EOF {
		err = nil
	}
	if r.IsMultiRange() {
		buf = buf[:0]
		multiRangeBodyEnd(&buf, r.boundary)
		nw, errw := w.Write(buf)
		if err == nil && nw != len(buf) {
			panic("BUG: buf returned (n, nil), where n != len(buf)")
		}
		sum += nw
		err = errw
	}
	return int64(sum), err
}

func multiRangeLength(sta, end []int, cl int, ct, bd string) int {
	sum := 0
	hf := true
	bufv := copyBufPool.Get()
	buf := bufv.([]byte)
	for i := range sta {
		buf = buf[:0]
		if i > 0 {
			hf = false
		}
		multiRangeBodyHeader(&buf, sta[i], end[i], cl, ct, bd, hf)
		sum += len(buf)
		sum += end[i] - sta[i]
	}
	buf = buf[:0]
	multiRangeBodyEnd(&buf, bd)
	sum += len(buf)
	copyBufPool.Put(bufv)
	return sum
}

func multiRangeBodyHeader(b *[]byte, sta, end, size int, ct, boundary string, hf bool) {
	if !hf {
		*b = append(*b, s2b("\r\n")...)
	}
	*b = append(*b, s2b(fmt.Sprintf("--%s\r\n", boundary))...)
	*b = append(*b, s2b(fmt.Sprintf("%s: %s\r\n", HeaderContentRange,
		fmt.Sprintf("bytes %d-%d/%d", sta, end-1, size)))...)
	*b = append(*b, s2b(fmt.Sprintf("%s: %s\r\n", HeaderContentType, ct))...)
	*b = append(*b, s2b("\r\n")...)
}

func multiRangeBodyEnd(b *[]byte, boundary string) {
	*b = append(*b, s2b(fmt.Sprintf("\r\n--%s--\r\n", boundary))...)
}

func randomBoundary() string {
	var buf [30]byte
	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", buf[:])
}
