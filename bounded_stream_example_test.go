package fasthttp_test

import (
	"bufio"
	"fmt"
	"io"

	"github.com/valyala/fasthttp"
)

func ExampleNewBoundedStreamReader() {
	var resp fasthttp.Response
	resp.SetBodyStream(fasthttp.NewBoundedStreamReader(func(w *bufio.Writer) {
		for _, part := range []string{"first\n", "second\n"} {
			if _, err := w.WriteString(part); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	}), -1)
	defer func() {
		if err := resp.CloseBodyStream(); err != nil {
			panic(err)
		}
	}()

	body, err := io.ReadAll(resp.BodyStream())
	if err != nil {
		panic(err)
	}
	fmt.Print(string(body))
	// Output:
	// first
	// second
}
