package fasthttputil_test

import (
	"testing"

	"github.com/valyala/fasthttp/fasthttputil"
)

func BenchmarkStringSlices(b *testing.B) {
	bytess := [][]byte{
		[]byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit"),
		[]byte("sed do eiusmod tempor incididunt ut labore et dolore magna aliqua"),
		[]byte(`Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris
				nisi ut aliquip ex ea commodo consequat.
				Duis aute irure dolor in reprehenderit in voluptate velit esse cillum
				dolore eu fugiat nulla pariatur. Excepteur sint occaecat cupidatat non proident,
				sunt in culpa qui officia deserunt mollit anim id est laborum`),
		[]byte("Sed ut perspiciatis"),
		[]byte("sed quia consequuntur magni dolores eos qui ratione voluptatem sequi nesciunt"),
		[]byte("Ut enim ad minima veniam, quis nostrum exercitationem ullam corporis suscipit"),
		[]byte("laboriosam, nisi ut aliquid ex ea commodi consequatur"),
		[]byte("Quis autem vel eum iure reprehenderit qui in ea voluptate velit esse quam nihil molestiae consequatur"),
		[]byte("vel illum qui dolorem eum fugiat quo voluptas nulla pariatur"),
	}

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			slices := fasthttputil.AcquireStringSlices()

			for _, bytes := range bytess {
				err := slices.WriteBytes(bytes)
				if err != nil {
					b.Fatal(err)
				}
			}

			for _ = range bytess {
				_, ok := slices.NextStringSlice()
				if !ok {
					b.Fatalf("unexpected interruption, last error: %v", slices.LastError())
				}
			}

			fasthttputil.ReleaseStringSlices(slices)
		}
	})
}
