package fasthttp

import (
	"bufio"
	"fmt"
	"io"
)

func ExampleNewBoundedStreamReader() {
	var resp Response
	resp.SetBodyStream(NewBoundedStreamReader(func(w *bufio.Writer) {
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
