package fasthttputil

import (
	"sync"

	"github.com/valyala/bytebufferpool"
)

var (
	stringSlicesPool sync.Pool
)

// AcquireStringSlices returns an empty StringSlice instance from pool.
//
// The returned StringSlice instance may be passed to ReleaseStringSlices
//  when it is no longer needed.
func AcquireStringSlices() *StringSlices {
	v := stringSlicesPool.Get()
	if v == nil {
		return &StringSlices{}
	}
	return v.(*StringSlices)
}

// ReleaseStringSlices returns slices acquired via AcquireStringSlices to pool.
func ReleaseStringSlices(slices *StringSlices) {
	slices.Reset()
	stringSlicesPool.Put(slices)
}

// StringSlices is used to convert byte slices to string slices,
// it only needs to allocate memory once.
type StringSlices struct {
	buffer *bytebufferpool.ByteBuffer

	lengths []int
	pos     int
	offset  int

	lastError error

	str string
}

// WriteBytes writes the byte slice of a string.
func (slices *StringSlices) WriteBytes(bytes []byte) error {
	if slices.lastError != nil {
		return nil
	}

	if slices.buffer == nil {
		slices.buffer = bytebufferpool.Get()
	}
	len, lastError := slices.buffer.Write(bytes)
	slices.lengths = append(slices.lengths, len)
	return lastError
}

// NextStringSlice returns next string slice.
// it returns empty string if StringSlices have no more string slices.
func (slices *StringSlices) NextStringSlice() (string, bool) {
	if slices.buffer == nil {
		return "", false
	}
	if slices.str == "" {
		slices.str = slices.buffer.String()
	}
	if slices.pos >= len(slices.lengths) {
		return "", false
	}

	end := slices.offset + slices.lengths[slices.pos]
	str := slices.str[slices.offset:end]

	slices.pos++
	slices.offset = end

	return str, true
}

// Number returns the number of string slices.
func (slices *StringSlices) Number() int {
	return len(slices.lengths)
}

// Remain returns the number of remaining readable string slices.
func (slices *StringSlices) Remain() int {
	return len(slices.lengths) - slices.pos
}

// LastError Return the last error of slices
func (slices *StringSlices) LastError() error {
	return slices.lastError
}

// Reset reset the StringSlices.
func (slices *StringSlices) Reset() {
	if slices.buffer != nil {
		bytebufferpool.Put(slices.buffer)
		slices.buffer = nil
	}
	slices.lengths = slices.lengths[:0]
	slices.pos = 0
	slices.offset = 0
	slices.lastError = nil
	slices.str = ""
}
