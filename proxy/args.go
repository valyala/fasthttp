package proxy

import "bytes"

// Args represents query arguments.
//
// It is forbidden copying Args instances. Create new instances instead
// and use CopyTo().
//
// Args instance MUST NOT be used from concurrently running goroutines.
type Args struct {
	noCopy noCopy

	args []argsKV
	buf  []byte
}

type argsKV struct {
	key   []byte
	value []byte
}

// Reset clears query args.
func (a *Args) Reset() {
	a.args = a.args[:0]
}

// Len returns the number of query args.
func (a *Args) Len() int {
	return len(a.args)
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

// SetBytesKV sets 'key=value' argument.
func (a *Args) SetBytesKV(key, value []byte) {
	a.args = setArgBytes(a.args, key, value)
}

func delAllArgsBytes(args []argsKV, key []byte) []argsKV {
	return delAllArgs(args, b2s(key))
}

func delAllArgs(args []argsKV, key string) []argsKV {
	for i, n := 0, len(args); i < n; i++ {
		kv := &args[i]
		if key == string(kv.key) {
			tmp := *kv
			copy(args[i:], args[i+1:])
			n--
			args[n] = tmp
			args = args[:n]
		}
	}
	return args
}

func setArgBytes(h []argsKV, key, value []byte) []argsKV {
	return setArg(h, b2s(key), b2s(value))
}

func setArg(h []argsKV, key, value string) []argsKV {
	n := len(h)
	for i := 0; i < n; i++ {
		kv := &h[i]
		if key == string(kv.key) {
			kv.value = append(kv.value[:0], value...)
			return h
		}
	}
	return appendArg(h, key, value)
}

func appendArgBytes(h []argsKV, key, value []byte) []argsKV {
	return appendArg(h, b2s(key), b2s(value))
}

func appendArg(args []argsKV, key, value string) []argsKV {
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

func peekArgBytes(h []argsKV, k []byte) []byte {
	for i, n := 0, len(h); i < n; i++ {
		kv := &h[i]
		if bytes.Equal(kv.key, k) {
			return kv.value
		}
	}
	return nil
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
