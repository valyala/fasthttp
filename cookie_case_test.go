package fasthttp

import "testing"

func TestCaseInsensitiveCompareASCII(t *testing.T) {
	t.Parallel()

	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			want := a == b ||
				(a >= 'A' && a <= 'Z' && a+'a'-'A' == b) ||
				(b >= 'A' && b <= 'Z' && b+'a'-'A' == a)
			if got := caseInsensitiveCompare([]byte{byte(a)}, []byte{byte(b)}); got != want {
				t.Fatalf("caseInsensitiveCompare(%#x, %#x) = %v, want %v", a, b, got, want)
			}
		}
	}
}
