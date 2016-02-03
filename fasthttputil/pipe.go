package fasthttputil

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

func newPipeConns() *pipeConns {
	pc := &pipeConns{}
	pc.c1.r = make(chan *byteBuffer, 1024)
	pc.c1.w = make(chan *byteBuffer, 1024)
	pc.c2.r = pc.c1.w
	pc.c2.w = pc.c1.r
	pc.c1.parent = pc
	pc.c2.parent = pc
	return pc
}

type pipeConns struct {
	c1 pipeConn
	c2 pipeConn
}

type pipeConn struct {
	r      chan *byteBuffer
	w      chan *byteBuffer
	b      *byteBuffer
	bb     []byte
	lock   sync.RWMutex
	closed bool
	parent *pipeConns
}

func (c *pipeConn) Write(p []byte) (int, error) {
	c.lock.RLock()
	if c.closed {
		c.lock.RUnlock()
		return 0, errors.New("connection closed")
	}

	b := acquireByteBuffer()
	b.b = append(b.b[:0], p...)
	c.w <- b

	c.lock.RUnlock()
	return len(p), nil
}

func (c *pipeConn) Read(p []byte) (int, error) {
	if len(c.bb) == 0 {
		releaseByteBuffer(c.b)
		c.b = nil
		b, ok := <-c.r
		if !ok {
			return 0, io.EOF
		}
		c.b = b
		c.bb = c.b.b
	}
	n := copy(p, c.bb)
	c.bb = c.bb[n:]

	return n, nil
}

func (c *pipeConn) Close() error {
	c.lock.Lock()
	if !c.closed {
		close(c.w)
		c.closed = true
		c.lock.Unlock()
		freeBuffers(c.parent)
		return nil
	}
	c.lock.Unlock()
	return errors.New("already closed")
}

func (p *pipeConn) LocalAddr() net.Addr {
	return pipeAddr(0)
}

func (p *pipeConn) RemoteAddr() net.Addr {
	return pipeAddr(0)
}

func (p *pipeConn) SetDeadline(t time.Time) error {
	return errors.New("deadline not supported")
}

func (p *pipeConn) SetReadDeadline(t time.Time) error {
	return p.SetDeadline(t)
}

func (p *pipeConn) SetWriteDeadline(t time.Time) error {
	return p.SetDeadline(t)
}

type pipeAddr int

func (pipeAddr) Network() string {
	return "pipe"
}

func (pipeAddr) String() string {
	return "pipe"
}

type byteBuffer struct {
	b []byte
}

func acquireByteBuffer() *byteBuffer {
	return byteBufferPool.Get().(*byteBuffer)
}

func releaseByteBuffer(b *byteBuffer) {
	if b != nil {
		byteBufferPool.Put(b)
	}
}

var byteBufferPool = &sync.Pool{
	New: func() interface{} {
		return &byteBuffer{}
	},
}

func freeBuffers(pc *pipeConns) {
	pc.c1.lock.RLock()
	pc.c2.lock.RLock()

	mustFree := pc.c1.closed && pc.c2.closed

	pc.c1.lock.RUnlock()
	pc.c2.lock.RUnlock()

	if mustFree {
		freeBufs(pc.c1.r)
		freeBufs(pc.c2.r)
	}
}

func freeBufs(ch chan *byteBuffer) {
	for b := range ch {
		releaseByteBuffer(b)
	}
}
