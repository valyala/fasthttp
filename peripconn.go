package fasthttp

import (
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
)

type perIPConnCounter struct {
	m    map[uint32]int
	lock sync.Mutex
}

func (cc *perIPConnCounter) Register(ip uint32) int {
	cc.lock.Lock()
	if cc.m == nil {
		cc.m = make(map[uint32]int)
	}
	n := cc.m[ip] + 1
	cc.m[ip] = n
	cc.lock.Unlock()
	return n
}

func (cc *perIPConnCounter) Unregister(ip uint32) {
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

	ip     uint32
	closed atomic.Bool
}

type perIPTLSConn struct {
	*tls.Conn

	perIPConnCounter *perIPConnCounter

	ip     uint32
	closed atomic.Bool
}

func newPerIPConn(conn net.Conn, ip uint32, counter *perIPConnCounter) net.Conn {
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

func getUint32IP(c net.Conn) uint32 {
	ip := getConnIP4(c)

	if len(ip) != 4 {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func getConnIP4(c net.Conn) net.IP {
	addr := c.RemoteAddr()
	ipAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return net.IPv4zero
	}
	return ipAddr.IP.To4()
}
