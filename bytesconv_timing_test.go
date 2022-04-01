package fasthttp

import (
	"bufio"
	"html"
	"net"
	"testing"

	"github.com/valyala/bytebufferpool"
)

func BenchmarkAppendHTMLEscape(b *testing.B) {
	sOrig := "<b>foobarbazxxxyyyzzz</b>"
	sExpected := string(AppendHTMLEscape(nil, sOrig))
	b.RunParallel(func(pb *testing.PB) {
		var buf []byte
		for pb.Next() {
			for i := 0; i < 10; i++ {
				buf = AppendHTMLEscape(buf[:0], sOrig)
				if string(buf) != sExpected {
					b.Fatalf("unexpected escaped string: %q. Expecting %q", buf, sExpected)
				}
			}
		}
	})
}

func BenchmarkHTMLEscapeString(b *testing.B) {
	sOrig := "<b>foobarbazxxxyyyzzz</b>"
	sExpected := html.EscapeString(sOrig)
	b.RunParallel(func(pb *testing.PB) {
		var s string
		for pb.Next() {
			for i := 0; i < 10; i++ {
				s = html.EscapeString(sOrig)
				if s != sExpected {
					b.Fatalf("unexpected escaped string: %q. Expecting %q", s, sExpected)
				}
			}
		}
	})
}

func BenchmarkParseIPv4(b *testing.B) {
	ipStr := []byte("123.145.167.189")
	b.RunParallel(func(pb *testing.PB) {
		var ip net.IP
		var err error
		for pb.Next() {
			ip, err = ParseIPv4(ip, ipStr)
			if err != nil {
				b.Fatalf("unexpected error: %v", err)
			}
		}
	})
}

func BenchmarkAppendIPv4(b *testing.B) {
	ip := net.ParseIP("123.145.167.189")
	b.RunParallel(func(pb *testing.PB) {
		var buf []byte
		for pb.Next() {
			buf = AppendIPv4(buf[:0], ip)
		}
	})
}

func BenchmarkWriteHexInt(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var w bytebufferpool.ByteBuffer
		bw := bufio.NewWriter(&w)
		i := 0
		for pb.Next() {
			writeHexInt(bw, i) //nolint:errcheck
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
				b.Fatalf("unexpected error: %v", err)
			}
			if n != 1234567 {
				b.Fatalf("unexpected result: %d. Expecting %q", n, buf)
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

func BenchmarkAppendUnquotedArgFastPath(b *testing.B) {
	src := []byte("foobarbaz no quoted chars fdskjsdf jklsdfdfskljd;aflskjdsaf fdsklj fsdkj fsdl kfjsdlk jfsdklj fsdfsdf sdfkflsd")
	b.RunParallel(func(pb *testing.PB) {
		var dst []byte
		for pb.Next() {
			dst = AppendUnquotedArg(dst[:0], src)
		}
	})
}

func BenchmarkAppendUnquotedArgSlowPath(b *testing.B) {
	src := []byte("D0%B4%20%D0%B0%D0%B2%D0%BB%D0%B4%D1%84%D1%8B%D0%B0%D0%BE%20%D1%84%D0%B2%D0%B6%D0%BB%D0%B4%D1%8B%20%D0%B0%D0%BE")
	b.RunParallel(func(pb *testing.PB) {
		var dst []byte
		for pb.Next() {
			dst = AppendUnquotedArg(dst[:0], src)
		}
	})
}
