package fasthttp

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"
)

func TestFSHandler(t *testing.T) {
	requestHandler := FSHandler(".", 0)
	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	f, err := os.Open(".")
	if err != nil {
		t.Fatalf("cannot open cwd: %s", err)
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		t.Fatalf("cannot read dirnames in cwd: %s", err)
	}

	for i := 0; i < 3; i++ {
		filesTested := 0
		for _, name := range filenames {
			if f, err = os.Open(name); err != nil {
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
