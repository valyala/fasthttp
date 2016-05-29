package proxy

import "bytes"

func getCookieKey(dst, src []byte) []byte {
	n := bytes.IndexByte(src, '=')
	if n >= 0 {
		src = src[:n]
	}
	return decodeCookieArg(dst, src, true)
}

func appendRequestCookieBytes(dst []byte, cookies []argsKV) []byte {
	for i, n := 0, len(cookies); i < n; i++ {
		kv := &cookies[i]
		if len(kv.key) > 0 {
			dst = AppendQuotedArg(dst, kv.key)
			dst = append(dst, '=')
		}
		dst = AppendQuotedArg(dst, kv.value)
		if i+1 < n {
			dst = append(dst, ';', ' ')
		}
	}
	return dst
}

func decodeCookieArg(dst, src []byte, decode bool) []byte {
	for len(src) > 0 && src[0] == ' ' {
		src = src[1:]
	}
	for len(src) > 0 && src[len(src)-1] == ' ' {
		src = src[:len(src)-1]
	}
	if !decode {
		return append(dst[:0], src...)
	}
	return decodeArg(dst, src, true)
}
