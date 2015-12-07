package fasthttp

import (
	"bufio"
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
// FSHandler caches requested file handles for FSHandlerCacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if root folder contains many files.
//
// Do not create multiple FSHandler instances for the same (root, stripSlashes)
// arguments - just reuse a single instance. Otherwise goroutine leak
// will occur.
func FSHandler(root string, stripSlashes int) RequestHandler {
	// strip trailing slashes from the root path
	for len(root) > 0 && root[len(root)-1] == '/' {
		root = root[:len(root)-1]
	}

	// serve files from the current working directory if root is empty
	if len(root) == 0 {
		root = "."
	}

	if stripSlashes < 0 {
		stripSlashes = 0
	}

	h := &fsHandler{
		root:         root,
		stripSlashes: stripSlashes,
		cache:        make(map[string]*fsFile),
	}
	go func() {
		for {
			time.Sleep(FSHandlerCacheDuration / 2)
			h.cleanCache()
		}
	}()
	return h.handleRequest
}

type fsHandler struct {
	root         string
	stripSlashes int
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

	t            time.Time
	readersCount int

	bigFiles     []*bigFileReader
	bigFilesLock sync.Mutex
}

func (ff *fsFile) Reader(incrementReaders bool) io.Reader {
	if incrementReaders {
		ff.h.cacheLock.Lock()
		ff.readersCount++
		ff.h.cacheLock.Unlock()
	}

	if ff.isBig() {
		return ff.bigFileReader()
	}
	return ff.smallFileReader()
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

const maxSmallFileSize = 4096

func (ff *fsFile) isBig() bool {
	return ff.contentLength > maxSmallFileSize && len(ff.dirIndex) == 0
}

func (ff *fsFile) bigFileReader() io.Reader {
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
		return r
	}

	f, err := os.Open(ff.f.Name())
	if err != nil {
		panic(fmt.Sprintf("BUG: cannot open already opened file %s: %s", ff.f.Name(), err))
	}
	return &bigFileReader{
		f:  f,
		ff: ff,
	}
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
	// fast path
	if rf, ok := w.(io.ReaderFrom); ok {
		// This is a hack for triggering sendfile path in bufio.Writer:
		// the buffer must be empty before calling ReadFrom.
		var n int
		if bw, ok := w.(*bufio.Writer); ok && bw.Buffered() > 0 {
			n = bw.Buffered()
			if err := bw.Flush(); err != nil {
				return 0, err
			}
		}
		nn, err := rf.ReadFrom(r.f)
		return nn + int64(n), err
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
		if t.Sub(ff.t) > FSHandlerCacheDuration {
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
	path := ctx.Path()
	path = stripPathSlashes(path, h.stripSlashes)

	if n := bytes.IndexByte(path, 0); n >= 0 {
		ctx.Logger().Printf("cannot serve path with nil byte at position %d: %q", n, path)
		ctx.Error("Are you a hacker?", StatusBadRequest)
		return
	}

	incrementReaders := true

	h.cacheLock.Lock()
	ff, ok := h.cache[string(path)]
	if ok {
		ff.readersCount++
		incrementReaders = false
	}
	h.cacheLock.Unlock()

	if !ok {
		pathStr := string(path)
		filePath := h.root + pathStr
		var err error
		ff, err = h.openFSFile(filePath)
		if err == errDirIndexRequired {
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

	ctx.SetBodyStream(ff.Reader(incrementReaders), ff.contentLength)
	ctx.SetContentType(ff.contentType)
}

var errDirIndexRequired = errors.New("directory index required")

func (h *fsHandler) createDirIndex(base *URI, filePath string) (*fsFile, error) {
	var buf bytes.Buffer
	w := &buf

	basePathEscaped := html.EscapeString(string(base.Path()))
	fmt.Fprintf(w, "<html><head><title>%s</title></head><body>", basePathEscaped)
	fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		fmt.Fprintf(w, `<li><a href="..">..</a></li>`)
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
		if !fi.IsDir() {
			auxStr = fmt.Sprintf("file, %d bytes", fi.Size())
		}
		fmt.Fprintf(w, `<li><a href="%s">%s</a>, %s, last modified %s</li>`,
			pathEscaped, html.EscapeString(name), auxStr, fi.ModTime())
	}

	fmt.Fprintf(w, "</ul></body></html>")
	dirIndex := w.Bytes()

	ff := &fsFile{
		h:             h,
		dirIndex:      dirIndex,
		contentType:   "text/html",
		contentLength: len(dirIndex),
	}
	return ff, nil
}

func (h *fsHandler) openFSFile(filePath string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	if stat.IsDir() {
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

	n := stat.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	ext := fileExtension(filePath)
	contentType := mime.TypeByExtension(ext)

	ff := &fsFile{
		h:             h,
		f:             f,
		contentType:   contentType,
		contentLength: contentLength,
	}
	return ff, nil
}

func stripPathSlashes(path []byte, stripSlashes int) []byte {
	// strip leading slashes
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

	// strip trailing slashes
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
