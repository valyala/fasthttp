package fasthttp_test

import (
	"bytes"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestArgsSortComparatorSign(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		compare func([]byte, []byte) int
	}{
		{"normalized", bytes.Compare},
		{"scaled", func(a, b []byte) int { return 7 * bytes.Compare(a, b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, keysOnly := range []bool{false, true} {
				var args fasthttp.Args
				args.Parse("z=last&a=z&a=a&c=middle&a=z")
				want := "a=a&a=z&a=z&c=middle&z=last"
				if keysOnly {
					args.SortKeys(tc.compare)
					want = "a=z&a=a&a=z&c=middle&z=last"
				} else {
					args.Sort(tc.compare)
				}
				if got := args.String(); got != want {
					t.Errorf("keysOnly=%v: got %q, want %q", keysOnly, got, want)
				}
			}

			var values fasthttp.Args
			values.Parse("a=z&a=a&a=m")
			values.Sort(tc.compare)
			if got := values.String(); got != "a=a&a=m&a=z" {
				t.Errorf("value-only sort: got %q", got)
			}

			var descending fasthttp.Args
			descending.Parse("a=a&z=last&a=z&c=middle")
			descending.Sort(func(a, b []byte) int { return tc.compare(b, a) })
			if got := descending.String(); got != "z=last&c=middle&a=z&a=a" {
				t.Errorf("descending sort: got %q", got)
			}
		})
	}
}

func TestArgsSortComparatorEqualStability(t *testing.T) {
	t.Parallel()

	for _, keysOnly := range []bool{false, true} {
		var args fasthttp.Args
		args.Parse("z=last&a=z&a=a&c=middle")
		compare := func(_, _ []byte) int { return 0 }
		if keysOnly {
			args.SortKeys(compare)
		} else {
			args.Sort(compare)
		}
		if got := args.String(); got != "z=last&a=z&a=a&c=middle" {
			t.Errorf("keysOnly=%v: got %q", keysOnly, got)
		}
	}
}
