package fasthttp

import (
	"sync"
)

const (
	defaultByteBufferSize = 128
)

// ByteBuffer provides byte buffer, which can be used with fasthttp API
// in order to minimize memory allocations.
//
// ByteBuffer may be used with functions appending data to the given []byte
// slice. See example code for details.
//
// Use AcquireByteBuffer for obtaining an empty byte buffer.
type ByteBuffer struct {

	// B is a byte buffer to use in append-like workloads.
	// See example code for details.
	B []byte
}

// Write implements io.Writer - it appends p to ByteBuffer.B
func (b *ByteBuffer) Write(p []byte) (int, error) {
	b.B = append(b.B, p...)
	return len(p), nil
}

// WriteString appends s to ByteBuffer.B
func (b *ByteBuffer) WriteString(s string) (int, error) {
	b.B = append(b.B, s...)
	return len(s), nil
}

// Set sets ByteBuffer.B to p
func (b *ByteBuffer) Set(p []byte) {
	b.B = append(b.B[:0], p...)
}

// SetString sets ByteBuffer.B to s
func (b *ByteBuffer) SetString(s string) {
	b.B = append(b.B[:0], s...)
}

// Reset makes ByteBuffer.B empty.
func (b *ByteBuffer) Reset() {
	b.B = b.B[:0]
}

// AcquireByteBuffer returns an empty byte buffer from the pool.
//
// Acquired byte buffer may be returned to the pool via ReleaseByteBuffer call.
// This reduces the number of memory allocations required for byte buffer
// management.
func AcquireByteBuffer() *ByteBuffer {
	return defaultByteBufferPool.Acquire()
}

// ReleaseByteBuffer returns byte buffer to the pool.
//
// ByteBuffer.B mustn't be touched after returning it to the pool.
// Otherwise data races occur.
func ReleaseByteBuffer(b *ByteBuffer) {
	defaultByteBufferPool.Release(b)
}

type byteBufferPool struct {
	pool sync.Pool
}

var defaultByteBufferPool byteBufferPool

func (p *byteBufferPool) Acquire() *ByteBuffer {
	v := p.pool.Get()
	if v == nil {
		return &ByteBuffer{
			B: make([]byte, 0, defaultByteBufferSize),
		}
	}
	return v.(*ByteBuffer)
}

func (p *byteBufferPool) Release(b *ByteBuffer) {
	b.B = b.B[:0]
	p.pool.Put(b)
}
