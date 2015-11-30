package fasthttp

import (
	"bufio"
	"bytes"
	"testing"
)

func BenchmarkInt2HexByte(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var i int
		for pb.Next() {
			i = 16
			for i > 0 {
				i--
				int2hexbyte(i)
			}
		}
	})
}

func BenchmarkHexByte2Int(b *testing.B) {
	buf := []byte("0123456789abcdefABCDEF")
	b.RunParallel(func(pb *testing.PB) {
		var c byte
		for pb.Next() {
			for _, c = range buf {
				hexbyte2int(c)
			}
		}
	})
}

func BenchmarkWriteHexInt(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var w bytes.Buffer
		bw := bufio.NewWriter(&w)
		i := 0
		for pb.Next() {
			writeHexInt(bw, i)
			i++
			if i > 0x7fffffff {
				i = 0
			}
			w.Reset()
			bw.Reset(&w)
		}
	})
}

func BenchmarkParseUint(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		buf := []byte("1234567")
		for pb.Next() {
			n, err := ParseUint(buf)
			if err != nil {
				b.Fatalf("unexpected error: %s", err)
			}
			if n != 1234567 {
				b.Fatalf("unexpected result: %d. Expecting %s", n, buf)
			}
		}
	})
}

func BenchmarkAppendUint(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var buf []byte
		i := 0
		for pb.Next() {
			buf = AppendUint(buf[:0], i)
			i++
			if i > 0x7fffffff {
				i = 0
			}
		}
	})
}

func BenchmarkLowercaseBytesNoop(b *testing.B) {
	src := []byte("foobarbaz_lowercased_all")
	b.RunParallel(func(pb *testing.PB) {
		s := make([]byte, len(src))
		for pb.Next() {
			copy(s, src)
			lowercaseBytes(s)
		}
	})
}

func BenchmarkLowercaseBytesAll(b *testing.B) {
	src := []byte("FOOBARBAZ_UPPERCASED_ALL")
	b.RunParallel(func(pb *testing.PB) {
		s := make([]byte, len(src))
		for pb.Next() {
			copy(s, src)
			lowercaseBytes(s)
		}
	})
}

func BenchmarkLowercaseBytesMixed(b *testing.B) {
	src := []byte("Foobarbaz_Uppercased_Mix")
	b.RunParallel(func(pb *testing.PB) {
		s := make([]byte, len(src))
		for pb.Next() {
			copy(s, src)
			lowercaseBytes(s)
		}
	})
}

func BenchmarkEqualBytesStrEq(b *testing.B) {
	s := "foobarbaraz"
	bs := []byte(s)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !EqualBytesStr(bs, s) {
				b.Fatalf("unexpected result: %q != %q", bs, s)
			}
		}
	})
}

func BenchmarkEqualBytesStrNe(b *testing.B) {
	s := "foobarbaraz"
	bs := []byte(s)
	bs[len(s)-1] = 'a'
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if EqualBytesStr(bs, s) {
				b.Fatalf("unexpected result: %q = %q", bs, s)
			}
		}
	})
}

func BenchmarkAppendBytesStr(b *testing.B) {
	s := "foobarbazbaraz"
	b.RunParallel(func(pb *testing.PB) {
		var dst []byte
		for pb.Next() {
			dst = AppendBytesStr(dst[:0], s)
		}
	})
}
