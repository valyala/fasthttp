package fasthttp

import (
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
)

// perIPKey identifies the client a connection comes from. Every address is
// held in its 16-byte form, so an IPv4 peer and the IPv4-mapped form of the
// same address are one client.
type perIPKey [16]byte

type perIPConnCounter struct {
	m    map[perIPKey]int
	lock sync.Mutex
}

func (cc *perIPConnCounter) Register(ip perIPKey) int {
	cc.lock.Lock()
	if cc.m == nil {
		cc.m = make(map[perIPKey]int)
	}
	n := cc.m[ip] + 1
	cc.m[ip] = n
	cc.lock.Unlock()
	return n
}

func (cc *perIPConnCounter) Unregister(ip perIPKey) {
	cc.lock.Lock()
	defer cc.lock.Unlock()
	if cc.m == nil {
		// developer safeguard
		panic("BUG: perIPConnCounter.Register() wasn't called")
	}
	// Drop the entry, otherwise the map keeps a key per distinct client IP forever.
	if n := cc.m[ip] - 1; n > 0 {
		cc.m[ip] = n
	} else {
		delete(cc.m, ip)
	}
}

// A per-IP wrapper is not recycled: Shutdown closes the connection from
// another goroutine while the serving one may still use the wrapper.
type perIPConn struct {
	net.Conn

	perIPConnCounter *perIPConnCounter

	ip     perIPKey
	closed atomic.Bool
}

type perIPTLSConn struct {
	*tls.Conn

	perIPConnCounter *perIPConnCounter

	ip     perIPKey
	closed atomic.Bool
}

func newPerIPConn(conn net.Conn, ip perIPKey, counter *perIPConnCounter) net.Conn {
	if tlsConn, ok := conn.(*tls.Conn); ok {
		return &perIPTLSConn{
			perIPConnCounter: counter,
			Conn:             tlsConn,
			ip:               ip,
		}
	}
	return &perIPConn{
		perIPConnCounter: counter,
		Conn:             conn,
		ip:               ip,
	}
}

func (c *perIPConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	err := c.Conn.Close()
	c.perIPConnCounter.Unregister(c.ip)
	return err
}

func (c *perIPTLSConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	err := c.Conn.Close()
	c.perIPConnCounter.Unregister(c.ip)
	return err
}

// getPerIPKey returns the counter key for the client c comes from. ok is
// false when the peer has no IP address to key on, which leaves the
// connection uncounted.
func getPerIPKey(c net.Conn) (key perIPKey, ok bool) {
	ip := getConnIP(c).To16()
	if ip == nil {
		return key, false
	}
	copy(key[:], ip)
	return key, true
}

// getConnIP returns the IP the connection comes from, or nil when its peer
// has no IP address.
func getConnIP(c net.Conn) net.IP {
	ipAddr, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return ipAddr.IP
}
