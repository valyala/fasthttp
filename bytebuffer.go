package fasthttp

import (
	"github.com/valyala/bytebufferpool"
)

var defaultByteBufferPool bytebufferpool.Pool

// NewByteBuffer returns an empty new byte buffer.
func NewByteBuffer() *bytebufferpool.ByteBuffer {
	return new(bytebufferpool.ByteBuffer)
}

// AcquireByteBuffer returns an empty byte buffer from the pool.
//
// Acquired byte buffer may be returned to the pool via ReleaseByteBuffer call.
// This reduces the number of memory allocations required for byte buffer
// management.
func AcquireByteBuffer() *bytebufferpool.ByteBuffer {
	return defaultByteBufferPool.Get()
}

// ReleaseByteBuffer returns byte buffer to the pool.
//
// ByteBuffer.B mustn't be touched after returning it to the pool.
// Otherwise data races occur.
func ReleaseByteBuffer(b *bytebufferpool.ByteBuffer) {
	defaultByteBufferPool.Put(b)
}
