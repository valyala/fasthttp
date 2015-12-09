package fasthttp

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"testing"
)

func TestNewStreamReader(t *testing.T) {
	r := NewStreamReader(func(w *bufio.Writer) {
		fmt.Fprintf(w, "Hello, world\n")
		fmt.Fprintf(w, "Line #2\n")
	})

	data, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	expectedData := "Hello, world\nLine #2\n"
	if string(data) != expectedData {
		t.Fatalf("unexpected data %q. Expecting %q", data, expectedData)
	}
}
