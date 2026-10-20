package fasthttputil

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// NewPipeConns returns new bi-directional connection pipe.
//
// PipeConns is NOT safe for concurrent use by multiple goroutines!
func NewPipeConns() *PipeConns {
	ch1 := make(chan *byteBuffer, 4)
	ch2 := make(chan *byteBuffer, 4)

	pc := &PipeConns{
		stopCh: make(chan struct{}),
	}
	pc.c1.rCh = ch1
	pc.c1.wCh = ch2
	pc.c2.rCh = ch2
	pc.c2.wCh = ch1
	pc.c1.pc = pc
	pc.c2.pc = pc
	return pc
}

// PipeConns provides bi-directional connection pipe,
// which use in-process memory as a transport.
//
// PipeConns must be created by calling NewPipeConns.
//
// PipeConns has the following additional features comparing to connections
// returned from net.Pipe():
//
//   - It is faster.
//   - It buffers Write calls, so there is no need to have concurrent goroutine
//     calling Read in order to unblock each Write call.
//   - It supports read and write deadlines.
//
// PipeConns is NOT safe for concurrent use by multiple goroutines!
type PipeConns struct {
	stopCh     chan struct{}
	c1         pipeConn
	c2         pipeConn
	stopChLock sync.Mutex
}

// SetAddresses sets the local and remote addresses for the connection.
func (pc *PipeConns) SetAddresses(localAddr1, remoteAddr1, localAddr2, remoteAddr2 net.Addr) {
	pc.c1.addrLock.Lock()
	defer pc.c1.addrLock.Unlock()

	pc.c2.addrLock.Lock()
	defer pc.c2.addrLock.Unlock()

	pc.c1.localAddr = localAddr1
	pc.c1.remoteAddr = remoteAddr1

	pc.c2.localAddr = localAddr2
	pc.c2.remoteAddr = remoteAddr2
}

// Conn1 returns the first end of bi-directional pipe.
//
// Data written to Conn1 may be read from Conn2.
// Data written to Conn2 may be read from Conn1.
func (pc *PipeConns) Conn1() net.Conn {
	return &pc.c1
}

// Conn2 returns the second end of bi-directional pipe.
//
// Data written to Conn2 may be read from Conn1.
// Data written to Conn1 may be read from Conn2.
func (pc *PipeConns) Conn2() net.Conn {
	return &pc.c2
}

// Close closes pipe connections.
func (pc *PipeConns) Close() error {
	pc.stopChLock.Lock()
	select {
	case <-pc.stopCh:
	default:
		close(pc.stopCh)
	}
	pc.stopChLock.Unlock()

	return nil
}

type pipeConn struct {
	localAddr  net.Addr
	remoteAddr net.Addr
	b          *byteBuffer

	rCh chan *byteBuffer
	wCh chan *byteBuffer
	pc  *PipeConns

	bb []byte

	readDeadline  pipeDeadline
	writeDeadline pipeDeadline

	addrLock sync.RWMutex
}

func (c *pipeConn) Write(p []byte) (int, error) {
	b := acquireByteBuffer()
	b.b = append(b.b[:0], p...)

	select {
	case <-c.pc.stopCh:
		releaseByteBuffer(b)
		return 0, ErrConnectionClosed
	default:
	}

	select {
	case c.wCh <- b:
	default:
		select {
		case c.wCh <- b:
		case <-c.writeDeadline.wait():
			releaseByteBuffer(b)
			return 0, ErrTimeout
		case <-c.pc.stopCh:
			releaseByteBuffer(b)
			return 0, ErrConnectionClosed
		}
	}

	return len(p), nil
}

func (c *pipeConn) WriteString(s string) (int, error) {
	return c.Write(s2b(s))
}

func (c *pipeConn) Read(p []byte) (int, error) {
	mayBlock := true
	nn := 0
	for len(p) > 0 {
		n, err := c.read(p, mayBlock)
		nn += n
		if err != nil {
			if !mayBlock && err == errWouldBlock {
				err = nil
			}
			return nn, err
		}
		p = p[n:]
		mayBlock = false
	}

	return nn, nil
}

func (c *pipeConn) read(p []byte, mayBlock bool) (int, error) {
	if len(c.bb) == 0 {
		if err := c.readNextByteBuffer(mayBlock); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.bb)
	c.bb = c.bb[n:]

	return n, nil
}

func (c *pipeConn) readNextByteBuffer(mayBlock bool) error {
	releaseByteBuffer(c.b)
	c.b = nil

	select {
	case c.b = <-c.rCh:
	default:
		if !mayBlock {
			return errWouldBlock
		}
		select {
		case c.b = <-c.rCh:
		case <-c.readDeadline.wait():
			// rCh may contain data when deadline is reached.
			// Read the data before returning ErrTimeout.
			select {
			case c.b = <-c.rCh:
			default:
				return ErrTimeout
			}
		case <-c.pc.stopCh:
			// rCh may contain data when stopCh is closed.
			// Read the data before returning EOF.
			select {
			case c.b = <-c.rCh:
			default:
				return io.EOF
			}
		}
	}

	c.bb = c.b.b
	return nil
}

var errWouldBlock = errors.New("would block")

// ErrConnectionClosed indicates that the underlying connection is closed. It could mean that the client has disconnected.
var ErrConnectionClosed = errors.New("fasthttputil: connection closed")

type timeoutError struct{}

func (e *timeoutError) Error() string {
	return "fasthttputil: timeout"
}

// Timeout implements the Timeout method of the net.Error interface.
// This allows for checks like:
//
//	if x, ok := err.(interface{ Timeout() bool }); ok && x.Timeout() {
func (e *timeoutError) Timeout() bool {
	return true
}

// ErrTimeout is returned from Read() or Write() on timeout.
var ErrTimeout = &timeoutError{}

func (c *pipeConn) Close() error {
	return c.pc.Close()
}

func (c *pipeConn) LocalAddr() net.Addr {
	c.addrLock.RLock()
	defer c.addrLock.RUnlock()

	if c.localAddr != nil {
		return c.localAddr
	}

	return pipeAddr(0)
}

func (c *pipeConn) RemoteAddr() net.Addr {
	c.addrLock.RLock()
	defer c.addrLock.RUnlock()

	if c.remoteAddr != nil {
		return c.remoteAddr
	}

	return pipeAddr(0)
}

func (c *pipeConn) SetDeadline(deadline time.Time) error {
	c.SetReadDeadline(deadline)  //nolint:errcheck
	c.SetWriteDeadline(deadline) //nolint:errcheck
	return nil
}

func (c *pipeConn) SetReadDeadline(deadline time.Time) error {
	c.readDeadline.set(deadline)
	return nil
}

func (c *pipeConn) SetWriteDeadline(deadline time.Time) error {
	c.writeDeadline.set(deadline)
	return nil
}

// pipeDeadline works like the one behind net.Pipe: wait returns a channel
// that is closed once the deadline has passed, so that pending operations
// wake up, and a later deadline replaces a closed channel with an open one.
// The channel is made when an operation first blocks or a deadline is first
// set, and the timer is kept while its channel is open.
type pipeDeadline struct {
	timer  *time.Timer   // Closes cancel when it fires.
	cancel chan struct{} // nil until needed.
	armed  bool          // Scheduled; cleared by set once it has synchronized with the timer.
	mu     sync.Mutex
}

func (d *pipeDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.armed {
		d.armed = false
		if !d.timer.Stop() {
			<-d.cancel // The timer fired; wait for it to close cancel.
		}
	}
	if d.cancel != nil && isClosedChan(d.cancel) {
		d.cancel, d.timer = nil, nil // The timer is bound to the closed channel.
	}
	if t.IsZero() {
		return
	}
	dur := time.Until(t)
	if dur <= 0 {
		if d.cancel == nil {
			d.cancel = closedChan
		} else {
			close(d.cancel)
			d.timer = nil
		}
		return
	}
	if d.cancel == nil {
		d.cancel = make(chan struct{})
	}
	if d.timer == nil {
		// The timer holds only the channel, so a closed pipe is not kept
		// alive until the deadline.
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
	} else {
		d.timer.Reset(dur)
	}
	d.armed = true
}

func (d *pipeDeadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel == nil {
		d.cancel = make(chan struct{})
	}
	return d.cancel
}

var closedChan = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

func isClosedChan(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
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
	return byteBufferPool.Get().(*byteBuffer) //nolint:forcetypeassert
}

func releaseByteBuffer(b *byteBuffer) {
	if b != nil {
		byteBufferPool.Put(b)
	}
}

var byteBufferPool = &sync.Pool{
	New: func() any {
		return &byteBuffer{
			b: make([]byte, 1024),
		}
	},
}
