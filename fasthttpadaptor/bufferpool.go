package fasthttpadaptor

import "sync"

// Use a minimum buffer size of 32 KiB.
const minBufferSize = 32 * 1024

var bufferPool = &sync.Pool{
	New: func() any {
		b := make([]byte, minBufferSize)
		return &b
	},
}

// acquireBuffer returns a pointer to a slice of 0 length and
// at least minBufferSize capacity.
func acquireBuffer() *[]byte {
	buf, ok := bufferPool.Get().(*[]byte)
	if !ok {
		panic("fasthttpadaptor: cannot get *[]byte from bufferPool")
	}

	*buf = (*buf)[:0]
	return buf
}

// releaseBuffer recycles the buffer for reuse.
func releaseBuffer(buf *[]byte) {
	bufferPool.Put(buf)
}
