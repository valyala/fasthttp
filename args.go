package fasthttp

import (
	"bytes"
	"errors"
	"io"
)

// Args represents query arguments.
//
// It is forbidden copying Args instances. Create new instances instead
// and use CopyTo().
//
// It is unsafe modifying/reading Args instance from concurrently
// running goroutines.
type Args struct {
	args  []argsKV
	bufKV argsKV
	buf   []byte
}

type argsKV struct {
	key   []byte
	value []byte
}

// Reset clears query args.
func (a *Args) Reset() {
	a.args = a.args[:0]
}

// CopyTo copies all args to dst.
func (a *Args) CopyTo(dst *Args) {
	dst.Reset()
	dst.args = copyArgs(dst.args, a.args)
}

// VisitAll calls f for each existing arg.
//
// f must not retain references to key and value after returning.
// Make key and/or value copies if you need storing them after returning.
func (a *Args) VisitAll(f func(key, value []byte)) {
	visitArgs(a.args, f)
}

// Len returns the number of query args.
func (a *Args) Len() int {
	return len(a.args)
}

// Parse parses the given string containing query args.
func (a *Args) Parse(s string) {
	a.buf = append(a.buf[:0], s...)
	a.ParseBytes(a.buf)
}

// ParseBytes parses the given b containing query args.
func (a *Args) ParseBytes(b []byte) {
	a.Reset()

	var s argsScanner
	s.b = b

	var kv *argsKV
	a.args, kv = allocArg(a.args)
	for s.next(kv) {
		if len(kv.key) > 0 || len(kv.value) > 0 {
			a.args, kv = allocArg(a.args)
		}
	}
	a.args = releaseArg(a.args)
}

// String returns string representation of query args.
func (a *Args) String() string {
	return string(a.QueryString())
}

// QueryString returns query string for the args.
//
// The returned value is valid until the next call to Args methods.
func (a *Args) QueryString() []byte {
	a.buf = a.AppendBytes(a.buf[:0])
	return a.buf
}

// AppendBytes appends query string to dst and returns the extended dst.
func (a *Args) AppendBytes(dst []byte) []byte {
	for i, n := 0, len(a.args); i < n; i++ {
		kv := &a.args[i]
		dst = AppendQuotedArg(dst, kv.key)
		if len(kv.value) > 0 {
			dst = append(dst, '=')
			dst = AppendQuotedArg(dst, kv.value)
		}
		if i+1 < n {
			dst = append(dst, '&')
		}
	}
	return dst
}

// WriteTo writes query string to w.
//
// WriteTo implements io.WriterTo interface.
func (a *Args) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(a.QueryString())
	return int64(n), err
}

// Del deletes argument with the given key from query args.
func (a *Args) Del(key string) {
	a.bufKV.key = append(a.bufKV.key[:0], key...)
	a.DelBytes(a.bufKV.key)
}

// DelBytes deletes argument with the given key from query args.
func (a *Args) DelBytes(key []byte) {
	a.args = delArg(a.args, key)
}

// Set sets 'key=value' argument.
func (a *Args) Set(key, value string) {
	a.bufKV.value = append(a.bufKV.value[:0], value...)
	a.SetBytesV(key, a.bufKV.value)
}

// SetBytesK sets 'key=value' argument.
func (a *Args) SetBytesK(key []byte, value string) {
	a.bufKV.value = append(a.bufKV.value[:0], value...)
	a.SetBytesKV(key, a.bufKV.value)
}

// SetBytesV sets 'key=value' argument.
func (a *Args) SetBytesV(key string, value []byte) {
	a.bufKV.key = append(a.bufKV.key[:0], key...)
	a.SetBytesKV(a.bufKV.key, value)
}

// SetBytesKV sets 'key=value' argument.
func (a *Args) SetBytesKV(key, value []byte) {
	a.args = setArg(a.args, key, value)
}

// Peek returns query arg value for the given key.
//
// Returned value is valid until the next Args call.
func (a *Args) Peek(key string) []byte {
	return peekArgStr(a.args, key)
}

// PeekBytes returns query arg value for the given key.
//
// Returned value is valid until the next Args call.
func (a *Args) PeekBytes(key []byte) []byte {
	return peekArgBytes(a.args, key)
}

// Has returns true if the given key exists in Args.
func (a *Args) Has(key string) bool {
	a.bufKV.key = append(a.bufKV.key[:0], key...)
	return a.HasBytes(a.bufKV.key)
}

// HasBytes returns true if the given key exists in Args.
func (a *Args) HasBytes(key []byte) bool {
	return hasArg(a.args, key)
}

// ErrNoArgValue is returned when Args value with the given key is missing.
var ErrNoArgValue = errors.New("no Args value for the given key")

// GetUint returns uint value for the given key.
func (a *Args) GetUint(key string) (int, error) {
	value := a.Peek(key)
	if len(value) == 0 {
		return -1, ErrNoArgValue
	}
	return ParseUint(value)
}

// SetUint sets uint value for the given key.
func (a *Args) SetUint(key string, value int) {
	a.bufKV.key = append(a.bufKV.key[:0], key...)
	a.SetUintBytes(a.bufKV.key, value)
}

// SetUintBytes sets uint value for the given key.
func (a *Args) SetUintBytes(key []byte, value int) {
	a.bufKV.value = AppendUint(a.bufKV.value[:0], value)
	a.SetBytesKV(key, a.bufKV.value)
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
	return ParseUfloat(value)
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

func visitArgs(args []argsKV, f func(k, v []byte)) {
	for i, n := 0, len(args); i < n; i++ {
		kv := &args[i]
		f(kv.key, kv.value)
	}
}

func copyArgs(dst, src []argsKV) []argsKV {
	if cap(dst) < len(src) {
		tmp := make([]argsKV, len(src))
		copy(tmp, dst)
		dst = tmp
	}
	n := len(src)
	dst = dst[:n]
	for i := 0; i < n; i++ {
		dstKV := &dst[i]
		srcKV := &src[i]
		dstKV.key = append(dstKV.key[:0], srcKV.key...)
		dstKV.value = append(dstKV.value[:0], srcKV.value...)
	}
	return dst
}

func delArg(args []argsKV, key []byte) []argsKV {
	for i, n := 0, len(args); i < n; i++ {
		kv := &args[i]
		if bytes.Equal(kv.key, key) {
			tmp := *kv
			copy(args[i:], args[i+1:])
			args[n-1] = tmp
			return args[:n-1]
		}
	}
	return args
}

func setArg(h []argsKV, key, value []byte) []argsKV {
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

func appendArg(args []argsKV, key, value []byte) []argsKV {
	var kv *argsKV
	args, kv = allocArg(args)
	kv.key = append(kv.key[:0], key...)
	kv.value = append(kv.value[:0], value...)
	return args
}

func allocArg(h []argsKV) ([]argsKV, *argsKV) {
	n := len(h)
	if cap(h) > n {
		h = h[:n+1]
	} else {
		h = append(h, argsKV{})
	}
	return h, &h[n]
}

func releaseArg(h []argsKV) []argsKV {
	return h[:len(h)-1]
}

func hasArg(h []argsKV, k []byte) bool {
	for i, n := 0, len(h); i < n; i++ {
		kv := &h[i]
		if bytes.Equal(kv.key, k) {
			return true
		}
	}
	return false
}

func peekArgBytes(h []argsKV, k []byte) []byte {
	for i, n := 0, len(h); i < n; i++ {
		kv := &h[i]
		if bytes.Equal(kv.key, k) {
			return kv.value
		}
	}
	return nil
}

func peekArgStr(h []argsKV, k string) []byte {
	for i, n := 0, len(h); i < n; i++ {
		kv := &h[i]
		if string(kv.key) == k {
			return kv.value
		}
	}
	return nil
}

type argsScanner struct {
	b []byte
}

func (s *argsScanner) next(kv *argsKV) bool {
	if len(s.b) == 0 {
		return false
	}

	isKey := true
	k := 0
	for i, c := range s.b {
		switch c {
		case '=':
			if isKey {
				isKey = false
				kv.key = decodeArg(kv.key, s.b[:i], true)
				k = i + 1
			}
		case '&':
			if isKey {
				kv.key = decodeArg(kv.key, s.b[:i], true)
				kv.value = kv.value[:0]
			} else {
				kv.value = decodeArg(kv.value, s.b[k:i], true)
			}
			s.b = s.b[i+1:]
			return true
		}
	}

	if isKey {
		kv.key = decodeArg(kv.key, s.b, true)
		kv.value = kv.value[:0]
	} else {
		kv.value = decodeArg(kv.value, s.b[k:], true)
	}
	s.b = s.b[len(s.b):]
	return true
}

func decodeArg(dst, src []byte, decodePlus bool) []byte {
	return decodeArgAppend(dst[:0], src, decodePlus)
}

func decodeArgAppend(dst, src []byte, decodePlus bool) []byte {
	for i, n := 0, len(src); i < n; i++ {
		c := src[i]
		if c == '%' {
			if i+2 >= n {
				return append(dst, src[i:]...)
			}
			x1 := hexbyte2int(src[i+1])
			x2 := hexbyte2int(src[i+2])
			if x1 < 0 || x2 < 0 {
				dst = append(dst, c)
			} else {
				dst = append(dst, byte(x1<<4|x2))
				i += 2
			}
		} else if decodePlus && c == '+' {
			dst = append(dst, ' ')
		} else {
			dst = append(dst, c)
		}
	}
	return dst
}
