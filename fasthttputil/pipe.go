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
	c1     pipeConn
	c2     pipeConn
	lock   sync.RWMutex
	closed bool
}

type pipeConn struct {
	parent *pipeConns
	r      chan *byteBuffer
	w      chan *byteBuffer
	b      *byteBuffer
	bb     []byte
}

func (c *pipeConn) Write(p []byte) (int, error) {
	c.parent.lock.RLock()
	if c.parent.closed {
		c.parent.lock.RUnlock()
		return 0, errors.New("connection closed")
	}

	b := acquireByteBuffer()
	b.b = append(b.b[:0], p...)
	c.w <- b

	c.parent.lock.RUnlock()
	return len(p), nil
}

func (c *pipeConn) Read(p []byte) (int, error) {
	c.parent.lock.RLock()
	if c.parent.closed {
		c.parent.lock.RUnlock()
		return 0, errors.New("connection closed")
	}

	if len(c.bb) == 0 {
		releaseByteBuffer(c.b)
		b, ok := <-c.r
		if !ok {
			c.parent.lock.RUnlock()
			return 0, io.EOF
		}
		c.b = b
		c.bb = c.b.b
	}
	n := copy(p, c.bb)
	c.bb = c.bb[n:]

	c.parent.lock.RUnlock()
	return n, nil
}

func (c *pipeConn) Close() error {
	var err error

	c.parent.lock.Lock()
	if !c.parent.closed {
		close(c.r)
		close(c.w)
		c.parent.closed = true
	} else {
		err = errors.New("already closed")
	}
	c.parent.lock.Unlock()

	return err
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
