package fasthttp

import "testing"

var swarTestFills = []byte{0x00, 0x01, 0x1f, 0x20, 0x40, 0x41, 0x5a, 0x5b, 0x7e, 0x7f, 0x80, 0xff}

// swarTestWords calls f with every word that has c in lane pos and fill in
// the other lanes, for every byte value c, lane pos and fill in swarTestFills.
func swarTestWords(t *testing.T, f func(word [8]byte, w uint64)) {
	t.Helper()

	for _, fill := range swarTestFills {
		for pos := range 8 {
			for c := range 256 {
				var word [8]byte
				for i := range word {
					word[i] = fill
				}
				word[pos] = byte(c)
				f(word, loadWord(word[:]))
			}
		}
	}
}

func TestAnyByteBelow(t *testing.T) {
	t.Parallel()

	for _, n := range []byte{1, 0x09, 0x20, 0x7f, 0x80} {
		swarTestWords(t, func(word [8]byte, w uint64) {
			exp := false
			for _, c := range word {
				if c < n {
					exp = true
				}
			}
			if got := anyByteBelow(w, n); got != exp {
				t.Fatalf("unexpected anyByteBelow(%q, %#x): %v. Expecting %v", word[:], n, got, exp)
			}
		})
	}
}

func TestAnyByteEqual(t *testing.T) {
	t.Parallel()

	for _, e := range []byte{0x00, 0x09, 0x41, 0x7f, 0x80, 0xff} {
		swarTestWords(t, func(word [8]byte, w uint64) {
			exp := false
			for _, c := range word {
				if c == e {
					exp = true
				}
			}
			if got := anyByteEqual(w, e); got != exp {
				t.Fatalf("unexpected anyByteEqual(%q, %#x): %v. Expecting %v", word[:], e, got, exp)
			}
		})
	}
}

func TestAnyByteIsCTL(t *testing.T) {
	t.Parallel()

	swarTestWords(t, func(word [8]byte, w uint64) {
		exp := false
		for _, c := range word {
			if c < 0x20 || c == 0x7f {
				exp = true
			}
		}
		if got := anyByteIsCTL(w); got != exp {
			t.Fatalf("unexpected anyByteIsCTL(%q): %v. Expecting %v", word[:], got, exp)
		}
	})
}

func TestLowercaseWord(t *testing.T) {
	t.Parallel()

	swarTestWords(t, func(word [8]byte, w uint64) {
		var exp [8]byte
		for i, c := range word {
			exp[i] = toLowerTable[c]
		}
		var got [8]byte
		storeWord(got[:], lowercaseWord(w))
		if got != exp {
			t.Fatalf("unexpected lowercaseWord(%q): %q. Expecting %q", word[:], got[:], exp[:])
		}
	})
}
