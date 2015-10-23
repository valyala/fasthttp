package fasthttp

import (
	"bytes"
	"errors"
)

// Args represents query arguments.
//
// It is forbidden copying Args instances. Create new instances instead.
type Args struct {
	args  []argsKV
	bufKV argsKV
	buf   []byte
}

type argsKV struct {
	key   []byte
	value []byte
}

// Clear clear query args.
func (a *Args) Clear() {
	a.args = a.args[:0]
}

// Len returns the number of query args.
func (a *Args) Len() int {
	return len(a.args)
}

// Parse parses the given string containing query args.
func (a *Args) Parse(s string) {
	a.buf = AppendBytesStr(a.buf[:0], s)
	a.ParseBytes(a.buf)
}

// ParseBytes parses the given b containing query args.
//
// It is safe modifying b buffer conntents after ParseBytes return.
func (a *Args) ParseBytes(b []byte) {
	a.Clear()

	var p argsParser
	p.Init(b)

	n := cap(a.args)
	a.args = a.args[:n]
	for i := 0; i < n; i++ {
		kv := &a.args[i]
		if !p.Next(kv) {
			for j := 0; j < i; j++ {
			}
			a.args = a.args[:i]
			return
		}
	}

	for {
		var kv argsKV
		if !p.Next(&kv) {
			return
		}
		a.args = append(a.args, kv)
	}
}

// String returns string representation of query args.
func (a *Args) String() string {
	a.buf = a.AppendBytes(a.buf[:0])
	return string(a.buf)
}

// AppendBytes appends query string to dst and returns dst
// (which may be newly allocated).
//
// It is safe modifying dst buffer after AppendBytes returns.
func (a *Args) AppendBytes(dst []byte) []byte {
	for i, n := 0, len(a.args); i < n; i++ {
		kv := &a.args[i]
		dst = appendQuotedArg(dst, kv.key)
		if len(kv.value) > 0 {
			dst = append(dst, '=')
			dst = appendQuotedArg(dst, kv.value)
		}
		if i+1 < n {
			dst = append(dst, '&')
		}
	}
	return dst
}

// Del deletes argument with the given key from query args.
func (a *Args) Del(key string) {
	for i, n := 0, len(a.args); i < n; i++ {
		kv := &a.args[i]
		if EqualBytesStr(kv.key, key) {
			tmp := *kv
			copy(a.args[i:], a.args[i+1:])
			a.args[n-1] = tmp
			a.args = a.args[:n-1]
			return
		}
	}
}

// Set sets 'key=value' argument.
func (a *Args) Set(key, value string) {
	a.bufKV.value = AppendBytesStr(a.bufKV.value[:0], value)
	a.SetBytes(key, a.bufKV.value)
}

// SetBytes sets 'key=value' argument.
//
// It is safe modifying valye buffer after SetBytes() return.
func (a *Args) SetBytes(key string, value []byte) {
	a.bufKV.key = AppendBytesStr(a.bufKV.key[:0], key)
	a.args = setKV(a.args, a.bufKV.key, value)
}

func setKV(h []argsKV, key, value []byte) []argsKV {
	n := len(h)
	for i := 0; i < n; i++ {
		kv := &h[i]
		if bytes.Equal(kv.key, key) {
			kv.value = append(kv.value[:0], value...)
			return h
		}
	}

	if cap(h) > n {
		h = h[:n+1]
		kv := &h[n]
		kv.key = append(kv.key[:0], key...)
		kv.value = append(kv.value[:0], value...)
		return h
	}

	var kv argsKV
	kv.key = append(kv.key, key...)
	kv.value = append(kv.value, value...)
	return append(h, kv)
}

func peekKV(h []argsKV, k []byte) []byte {
	for i, n := 0, len(h); i < n; i++ {
		kv := &h[i]
		if bytes.Equal(k, kv.key) {
			return kv.value
		}
	}
	return nil
}

// Get returns query arg value for the given key.
//
// Each Get call allocates memory for returned string,
// so consider using Peek() instead.
func (a *Args) Get(key string) string {
	return string(a.Peek(key))
}

// Peek returns query arg value for the given key.
//
// Returned value is valid until the next Args call.
func (a *Args) Peek(key string) []byte {
	a.bufKV.key = AppendBytesStr(a.bufKV.key[:0], key)
	return peekKV(a.args, a.bufKV.key)
}

// Has returns true if the given key exists in Args.
func (a *Args) Has(key string) bool {
	return a.Peek(key) != nil
}

// ErrNoArgValue is returned when value with the given key is missing.
var ErrNoArgValue = errors.New("No value for the given key")

// GetUint returns uint value for the given key.
func (a *Args) GetUint(key string) (int, error) {
	value := a.Peek(key)
	if len(value) == 0 {
		return -1, ErrNoArgValue
	}
	return parseUint(value)
}

// GetUintOrZero returns uint value for the given key.
//
// Zero (0) is returned on error.
func (a *Args) GetUintOrZero(key string) int {
	n, err := a.GetUint(key)
	if err != nil {
		n = 0
	}
	return n
}

// GetUfloat returns ufloat value for the given key.
func (a *Args) GetUfloat(key string) (float64, error) {
	value := a.Peek(key)
	if len(value) == 0 {
		return -1, ErrNoArgValue
	}
	return parseUfloat(value)
}

// GetUfloatOrZero returns ufloat value for the given key.
//
// Zero (0) is returned on error.
func (a *Args) GetUfloatOrZero(key string) float64 {
	f, err := a.GetUfloat(key)
	if err != nil {
		f = 0
	}
	return f
}

// EqualBytesStr returns true if string(b) == s.
//
// It doesn't allocate memory unlike string(b) do.
func EqualBytesStr(b []byte, s string) bool {
	if len(s) != len(b) {
		return false
	}
	for i, n := 0, len(s); i < n; i++ {
		if s[i] != b[i] {
			return false
		}
	}
	return true
}

// AppendBytesStr appends src to dst and returns dst
// (which may be newly allocated).
func AppendBytesStr(dst []byte, src string) []byte {
	for i, n := 0, len(src); i < n; i++ {
		dst = append(dst, src[i])
	}
	return dst
}

type argsParser struct {
	b []byte
}

func (p *argsParser) Init(buf []byte) {
	p.b = buf
}

func (p *argsParser) Next(kv *argsKV) bool {
	for {
		if !p.next(kv) {
			return false
		}
		if len(kv.key) > 0 || len(kv.value) > 0 {
			if kv.key == nil {
				kv.key = []byte{}
			}
			if kv.value == nil {
				kv.value = []byte{}
			}
			return true
		}
	}
}

func (p *argsParser) next(kv *argsKV) bool {
	if len(p.b) == 0 {
		return false
	}

	n := bytes.IndexByte(p.b, '&')
	var b []byte
	if n < 0 {
		b = p.b
		p.b = p.b[len(p.b):]
	} else {
		b = p.b[:n]
		p.b = p.b[n+1:]
	}

	n = bytes.IndexByte(b, '=')
	if n < 0 {
		kv.key = decodeArg(kv.key[:0], b, true)
		kv.value = kv.value[:0]
	} else {
		kv.key = decodeArg(kv.key[:0], b[:n], true)
		kv.value = decodeArg(kv.value[:0], b[n+1:], true)
	}
	return true
}

func decodeArg(dst, src []byte, decodePlus bool) []byte {
	for i, n := 0, len(src); i < n; i++ {
		c := src[i]
		switch c {
		case '+':
			if decodePlus {
				c = ' '
			}
			dst = append(dst, c)
		case '%':
			if i+2 >= n {
				return append(dst, src[i:]...)
			}
			x1 := unhex(src[i+1])
			x2 := unhex(src[i+2])
			if x1 < 0 || x2 < 0 {
				dst = append(dst, c)
			} else {
				dst = append(dst, byte(x1<<4|x2))
				i += 2
			}
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

func unhex(c byte) int {
	if c >= '0' && c <= '9' {
		return int(c - '0')
	}
	if c >= 'a' && c <= 'f' {
		return 10 + int(c-'a')
	}
	if c >= 'A' && c <= 'F' {
		return 10 + int(c-'A')
	}
	return -1
}

func appendQuotedArg(dst, v []byte) []byte {
	for _, c := range v {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '/' || c == '.' {
			dst = append(dst, c)
		} else {
			dst = append(dst, '%', hexChar(c>>4), hexChar(c&15))
		}
	}
	return dst
}

func hexChar(c byte) byte {
	if c < 10 {
		return '0' + c
	}
	return c - 10 + 'A'
}
