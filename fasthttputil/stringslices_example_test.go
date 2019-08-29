package fasthttputil_test

import (
	"fmt"

	"github.com/valyala/fasthttp/fasthttputil"
)

func ExampleStringSlices() {
	ss := fasthttputil.AcquireStringSlices()
	ss.WriteBytes([]byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit"))
	ss.WriteBytes([]byte("sed do eiusmod tempor incididunt ut labore et dolore magna aliqua"))

	s1, _ := ss.NextStringSlice()
	s2, _ := ss.NextStringSlice()

	fasthttputil.ReleaseStringSlices(ss)

	fmt.Println(s1)
	fmt.Println(s2)

	// Output:
	// Lorem ipsum dolor sit amet, consectetur adipiscing elit
	// sed do eiusmod tempor incididunt ut labore et dolore magna aliqua
}
