package fasthttp

import (
	"bytes"
	"io/ioutil"
	"os"
	"sort"
	"testing"
	"time"
)

func TestFSHandlerSingleThread(t *testing.T) {
	requestHandler := FSHandler(".", 0)

	f, err := os.Open(".")
	if err != nil {
		t.Fatalf("cannot open cwd: %s", err)
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		t.Fatalf("cannot read dirnames in cwd: %s", err)
	}
	sort.Sort(sort.StringSlice(filenames))

	for i := 0; i < 3; i++ {
		fsHandlerTest(t, requestHandler, filenames)
	}
}

func TestFSHandlerConcurrent(t *testing.T) {
	requestHandler := FSHandler(".", 0)

	f, err := os.Open(".")
	if err != nil {
		t.Fatalf("cannot open cwd: %s", err)
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		t.Fatalf("cannot read dirnames in cwd: %s", err)
	}
	sort.Sort(sort.StringSlice(filenames))

	concurrency := 10
	ch := make(chan struct{}, concurrency)
	for j := 0; j < concurrency; j++ {
		go func() {
			for i := 0; i < 3; i++ {
				fsHandlerTest(t, requestHandler, filenames)
			}
			ch <- struct{}{}
		}()
	}

	for j := 0; j < concurrency; j++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}
}

func fsHandlerTest(t *testing.T, requestHandler RequestHandler, filenames []string) {
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)
	ctx.Request.Header.SetHost("foobar.com")

	filesTested := 0
	for _, name := range filenames {
		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("cannot open file %q: %s", name, err)
		}
		stat, err := f.Stat()
		if err != nil {
			t.Fatalf("cannot get file stat %q: %s", name, err)
		}
		if stat.IsDir() {
			f.Close()
			continue
		}
		data, err := ioutil.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("cannot read file contents %q: %s", name, err)
		}

		ctx.URI().Update(name)
		requestHandler(&ctx)
		if ctx.Response.bodyStream == nil {
			t.Fatalf("response body stream must be non-empty")
		}
		body, err := ioutil.ReadAll(ctx.Response.bodyStream)
		if err != nil {
			t.Fatalf("error when reading response body stream: %s", err)
		}
		if !bytes.Equal(body, data) {
			t.Fatalf("unexpected body returned: %q. Expecting %q", body, data)
		}
		filesTested++
		if filesTested >= 10 {
			break
		}
	}
}
func TestStripPathSlashes(t *testing.T) {
	testStripPathSlashes(t, "", 0, "")
	testStripPathSlashes(t, "", 10, "")
	testStripPathSlashes(t, "/", 0, "")
	testStripPathSlashes(t, "/", 1, "")
	testStripPathSlashes(t, "/", 10, "")
	testStripPathSlashes(t, "/foo/bar/baz", 0, "/foo/bar/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 1, "/bar/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 2, "/baz")
	testStripPathSlashes(t, "/foo/bar/baz", 3, "")
	testStripPathSlashes(t, "/foo/bar/baz", 10, "")

	// trailing slash
	testStripPathSlashes(t, "/foo/bar/", 0, "/foo/bar")
	testStripPathSlashes(t, "/foo/bar/", 1, "/bar")
	testStripPathSlashes(t, "/foo/bar/", 2, "")
	testStripPathSlashes(t, "/foo/bar/", 3, "")
}

func testStripPathSlashes(t *testing.T, path string, stripSlashes int, expectedPath string) {
	s := stripPathSlashes([]byte(path), stripSlashes)
	if string(s) != expectedPath {
		t.Fatalf("unexpected path after stripping %q with stripSlashes=%d: %q. Expecting %q", path, stripSlashes, s, expectedPath)
	}
}

func TestFileExtension(t *testing.T) {
	testFileExtension(t, "foo.bar", ".bar")
	testFileExtension(t, "foobar", "")
	testFileExtension(t, "foo.bar.baz", ".baz")
	testFileExtension(t, "", "")
	testFileExtension(t, "/a/b/c.d/efg.jpg", ".jpg")
}

func testFileExtension(t *testing.T, path, expectedExt string) {
	ext := fileExtension(path)
	if ext != expectedExt {
		t.Fatalf("unexpected file extension for file %q: %q. Expecting %q", path, ext, expectedExt)
	}
}
