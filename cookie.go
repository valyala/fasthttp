package fasthttp

import (
	"bytes"
)

func appendCookieBytes(dst []byte, cookies []argsKV) []byte {
	for i, n := 0, len(cookies); i < n; i++ {
		kv := &cookies[i]
		dst = appendQuotedArg(dst, kv.key)
		dst = append(dst, '=')
		dst = appendQuotedArg(dst, kv.value)
		if i+1 < n {
			dst = append(dst, ';', ' ')
		}
	}
	return dst
}

func parseCookies(cookies []argsKV, src []byte, kv *argsKV) []argsKV {
	var s cookieScanner
	s.b = src
	for s.next(kv) {
		cookies = setArg(cookies, kv.key, kv.value)
	}
	return cookies
}

type cookieScanner struct {
	b []byte
}

func (s *cookieScanner) next(kv *argsKV) bool {
	if len(s.b) == 0 {
		return false
	}

	var b []byte
	n := bytes.IndexByte(s.b, ';')
	if n < 0 {
		b = s.b
		s.b = s.b[len(s.b):]
	} else {
		b = s.b[:n]
		s.b = s.b[n+1:]
	}

	n = bytes.IndexByte(b, '=')
	if n < 0 {
		kv.key = kv.key[:0]
		kv.value = decodeCookieArg(kv.value[:0], b)
		return true
	}

	kv.key = decodeCookieArg(kv.key[:0], b[:n])
	kv.value = decodeCookieArg(kv.value[:0], b[n+1:])
	return true
}

func decodeCookieArg(dst, src []byte) []byte {
	for len(src) > 0 && src[0] == ' ' {
		src = src[1:]
	}
	for len(src) > 0 && src[len(src)-1] == ' ' {
		src = src[:len(src)-1]
	}
	return decodeArg(dst, src, true)
}
