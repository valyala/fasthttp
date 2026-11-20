package fasthttp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// proxyHeaderTimeout bounds the wait for a PROXY protocol header.
const proxyHeaderTimeout = 5 * time.Second

// proxyV1MaxLen is the longest version 1 header, CRLF included.
const proxyV1MaxLen = 107

var (
	errProxyHeader  = errors.New("malformed PROXY protocol header")
	errProxyTLSConn = errors.New("cannot read the PROXY protocol header from under the TLS connection")

	proxyV2Signature = []byte("\r\n\r\n\x00\r\nQUIT\n")
	strProxyV1       = []byte("PROXY ")
	strProxyV1TCP4   = []byte("PROXY TCP4 ")
	strProxyV1TCP6   = []byte("PROXY TCP6 ")
)

// readProxyHeader reads the PROXY protocol header r starts with and not a
// byte past it. The addresses are nil when the header carries none to use.
// The error is io.EOF only if r ended before sending anything.
func readProxyHeader(r io.Reader) (remote, local net.Addr, err error) {
	var buf [proxyV1MaxLen + 1]byte
	// No header is shorter than the version 2 signature.
	if _, err := io.ReadFull(r, buf[:len(proxyV2Signature)]); err != nil {
		return nil, nil, err
	}
	switch {
	case bytes.Equal(buf[:len(proxyV2Signature)], proxyV2Signature):
		return readProxyHeaderV2(r, buf[:])
	case bytes.HasPrefix(buf[:], strProxyV1):
		return readProxyHeaderV1(r, buf[:])
	}
	return nil, nil, errProxyHeader
}

// proxyReadFull fills b with header bytes after the first ones, where running
// out of them means a truncated header.
func proxyReadFull(r io.Reader, b []byte) error {
	_, err := io.ReadFull(r, b)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func readProxyHeaderV1(r io.Reader, buf []byte) (remote, local net.Addr, err error) {
	n := len(proxyV2Signature)
	for {
		need := proxyV1Need(buf[:n])
		if need == 0 {
			break
		}
		if need < 0 || n+need > proxyV1MaxLen {
			return nil, nil, errProxyHeader
		}
		if err := proxyReadFull(r, buf[n:n+need]); err != nil {
			return nil, nil, err
		}
		n += need
	}

	line := buf[len(strProxyV1) : n-2]
	if rest, ok := bytes.CutPrefix(line, []byte("UNKNOWN")); ok && (len(rest) == 0 || rest[0] == ' ') {
		return nil, nil, nil
	}
	f := bytes.Split(line, strSpace)
	if len(f) != 5 {
		return nil, nil, errProxyHeader
	}
	var is4 bool
	switch string(f[0]) {
	case "TCP4":
		is4 = true
	case "TCP6":
	default:
		return nil, nil, errProxyHeader
	}
	src, ok := parseProxyV1Addr(f[1], f[3], is4)
	if !ok {
		return nil, nil, errProxyHeader
	}
	dst, ok := parseProxyV1Addr(f[2], f[4], is4)
	if !ok {
		return nil, nil, errProxyHeader
	}
	return src, dst, nil
}

// proxyV1Need returns how many more bytes a valid version 1 line starting
// with b has at the least: 0 once b ends the line, -1 if no valid line starts
// with b. Reading just that many keeps what follows the header unread.
func proxyV1Need(b []byte) int {
	fields := 0
	tcp := bytes.HasPrefix(b, strProxyV1TCP4) || bytes.HasPrefix(b, strProxyV1TCP6)
	if tcp {
		// Each address and port still to come takes a space and a byte.
		if fields = 5 - bytes.Count(b, strSpace); fields < 0 {
			return -1
		}
	}
	switch last := b[len(b)-1]; {
	case last == '\n' && b[len(b)-2] == '\r':
		return 0
	case last == '\r':
		return 1
	case tcp && last == ' ':
		return 2*fields + 3
	}
	return 2*fields + 2
}

func parseProxyV1Addr(ip, port []byte, is4 bool) (*net.TCPAddr, bool) {
	addr, err := netip.ParseAddr(b2s(ip))
	if err != nil || addr.Is4() != is4 || addr.Zone() != "" {
		return nil, false
	}
	if len(port) > 1 && port[0] == '0' {
		return nil, false
	}
	p, err := strconv.ParseUint(b2s(port), 10, 16)
	if err != nil {
		return nil, false
	}
	return &net.TCPAddr{IP: addr.AsSlice(), Port: int(p)}, true
}

func readProxyHeaderV2(r io.Reader, buf []byte) (remote, local net.Addr, err error) {
	fixed := len(proxyV2Signature) + 4
	if err := proxyReadFull(r, buf[len(proxyV2Signature):fixed]); err != nil {
		return nil, nil, err
	}
	verCmd, famProto := buf[fixed-4], buf[fixed-3]
	n := int(binary.BigEndian.Uint16(buf[fixed-2:]))
	if verCmd>>4 != 2 {
		return nil, nil, errProxyHeader
	}

	var addrLen int
	switch verCmd & 0x0f {
	case 0:
		// LOCAL: the address block is ignored, its family included.
	case 1:
		fam, proto := famProto>>4, famProto&0x0f
		if fam > 3 || proto > 2 {
			return nil, nil, errProxyHeader
		}
		if proto != 0 {
			addrLen = [...]int{0, 12, 36, 216}[fam]
		}
		if n < addrLen {
			return nil, nil, errProxyHeader
		}
		if proto != 1 || fam == 3 {
			// Only TCP over IPv4 and IPv6 is taken; the rest falls back to
			// the connection's own addresses.
			addrLen = 0
		}
	default:
		return nil, nil, errProxyHeader
	}

	if addrLen > 0 {
		a := buf[fixed : fixed+addrLen]
		if err := proxyReadFull(r, a); err != nil {
			return nil, nil, err
		}
		n -= addrLen
		ipLen := (addrLen - 4) / 2
		remote = &net.TCPAddr{
			IP:   bytes.Clone(a[:ipLen]),
			Port: int(binary.BigEndian.Uint16(a[2*ipLen:])),
		}
		local = &net.TCPAddr{
			IP:   bytes.Clone(a[ipLen : 2*ipLen]),
			Port: int(binary.BigEndian.Uint16(a[2*ipLen+2:])),
		}
	}
	// Skip the TLVs, or the addresses not taken.
	for n > 0 {
		m := min(n, len(buf))
		if err := proxyReadFull(r, buf[:m]); err != nil {
			return nil, nil, err
		}
		n -= m
	}
	return remote, local, nil
}

type proxyAddrs struct {
	remote net.Addr
	local  net.Addr
}

// proxyConn reports the addresses the connection's PROXY protocol header
// carried and holds its MaxConnsPerIP count.
type proxyConn struct {
	net.Conn

	addrs atomic.Pointer[proxyAddrs]

	mu      sync.Mutex
	counter *perIPConnCounter
	key     perIPKey
	closed  bool

	// expiring is held for a closeAfter timer until it is stopped or done.
	expiring sync.WaitGroup
}

type netConner interface {
	NetConn() net.Conn
}

// proxyTLSConn keeps a TLS connection recognizable as one.
type proxyTLSConn struct {
	proxyConn
	tlsConn
}

func newProxyConn(c net.Conn) net.Conn {
	if tc, ok := c.(tlsConn); ok {
		if _, ok := c.(netConner); ok {
			return &proxyTLSConn{proxyConn: proxyConn{Conn: c}, tlsConn: tc}
		}
	}
	return &proxyConn{Conn: c}
}

func unwrapProxyConn(c net.Conn) *proxyConn {
	switch c := c.(type) {
	case *proxyConn:
		return c
	case *proxyTLSConn:
		return &c.proxyConn
	}
	return nil
}

// NetConn returns the connection under TLS, as tls.Conn does.
func (c *proxyTLSConn) NetConn() net.Conn {
	return c.Conn.(netConner).NetConn() //nolint:forcetypeassert
}

// WrappedConn returns the connection the wrapper is around, as perIPConn's
// does.
func (c *proxyConn) WrappedConn() net.Conn {
	return c.Conn
}

func (c *proxyConn) RemoteAddr() net.Addr {
	if a := c.addrs.Load(); a != nil {
		return a.remote
	}
	return c.Conn.RemoteAddr()
}

func (c *proxyConn) LocalAddr() net.Addr {
	if a := c.addrs.Load(); a != nil {
		return a.local
	}
	return c.Conn.LocalAddr()
}

func (c *proxyConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	counter, key := c.counter, c.key
	c.mu.Unlock()

	err := c.Conn.Close()
	if counter != nil {
		counter.Unregister(key)
	}
	return err
}

// closeAfter starts a timer that in d sets the deadline of the read or write
// it bounds and closes c. expired is to be called for it.
func (c *proxyConn) closeAfter(d time.Duration, setDeadline func(time.Time) error) *time.Timer {
	c.expiring.Add(1)
	return time.AfterFunc(d, func() {
		// Closing a hijacked connection may leave it blocked.
		setDeadline(time.Now()) //nolint:errcheck
		c.Close()
		c.expiring.Done()
	})
}

// expired stops a closeAfter timer and reports whether it fired, once it is
// done if so.
func (c *proxyConn) expired(t *time.Timer) bool {
	if t.Stop() {
		c.expiring.Done()
		return false
	}
	c.expiring.Wait()
	return true
}

// countPerIP counts c under its remote address until it is closed. It
// returns ErrPerIPConnLimit if that takes the address over limit.
func (c *proxyConn) countPerIP(counter *perIPConnCounter, limit int) error {
	key, ok := getPerIPKey(c)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if counter.Register(key) > limit {
		counter.Unregister(key)
		return ErrPerIPConnLimit
	}
	c.counter, c.key = counter, key
	return nil
}

// acceptProxyHeader reads the PROXY protocol header c starts with, from under
// TLS if c is a TLS connection, and counts c against MaxConnsPerIP under the
// address it carries. It returns io.EOF if the peer left without sending
// anything and ErrPerIPConnLimit after closing a connection over the limit.
func (s *Server) acceptProxyHeader(c net.Conn) error {
	pc := unwrapProxyConn(c)
	if pc == nil {
		return nil
	}
	src := pc.Conn
	if _, ok := src.(tlsConn); ok {
		nc, ok := src.(netConner)
		if !ok {
			return errProxyTLSConn
		}
		if src = nc.NetConn(); src == nil {
			return errProxyTLSConn
		}
	}

	timeout := proxyHeaderTimeout
	if s.ReadTimeout > 0 && s.ReadTimeout < timeout {
		timeout = s.ReadTimeout
	}
	// A read deadline would replace the caller's.
	timer := pc.closeAfter(timeout, src.SetReadDeadline)
	remote, local, err := readProxyHeader(src)
	if pc.expired(timer) {
		return fmt.Errorf("cannot read PROXY protocol header: %w", os.ErrDeadlineExceeded)
	}
	if err != nil {
		if err != io.EOF && err != errProxyHeader {
			err = fmt.Errorf("cannot read PROXY protocol header: %w", err)
		}
		return err
	}
	if remote != nil {
		pc.addrs.Store(&proxyAddrs{remote: remote, local: local})
	}

	if s.MaxConnsPerIP <= 0 {
		return nil
	}
	err = pc.countPerIP(&s.perIPConnCounter, s.MaxConnsPerIP)
	if err == ErrPerIPConnLimit {
		// A TLS connection would have to finish its handshake first.
		if _, isTLS := c.(tlsConn); !isTLS {
			timer := pc.closeAfter(proxyHeaderTimeout, pc.SetWriteDeadline)
			s.writeFastError(c, StatusTooManyRequests, "The number of connections from your ip exceeds MaxConnsPerIP")
			pc.expired(timer)
		}
		c.Close()
	}
	return err
}

// logProxyPerIPLimit logs a connection over the MaxConnsPerIP limit, once a
// minute at the most like the accept loop does.
func (s *Server) logProxyPerIPLimit(c net.Conn) {
	last := s.proxyPerIPErrorTime.Load()
	now := time.Now().UnixNano()
	if now-last > int64(time.Minute) && s.proxyPerIPErrorTime.CompareAndSwap(last, now) {
		s.logger().Printf("The number of connections from %s exceeds MaxConnsPerIP=%d",
			getConnIP(c), s.MaxConnsPerIP)
	}
}
