package fasthttp

import (
	"crypto/tls"
	"net"
	"sync"
)

type perIPConnCounter struct {
	perIPConnPool    sync.Pool
	perIPTLSConnPool sync.Pool
	m                map[uint32]int
	lock             sync.Mutex
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
	n := cc.m[ip] - 1
	if n < 0 {
		n = 0
	}
	cc.m[ip] = n
}

type perIPConn struct {
	net.Conn

	perIPConnCounter *perIPConnCounter

	ip uint32
}

type perIPTLSConn struct {
	*tls.Conn

	perIPConnCounter *perIPConnCounter

	ip uint32
}

func acquirePerIPConn(conn net.Conn, ip uint32, counter *perIPConnCounter) net.Conn {
	if tlsConn, ok := conn.(*tls.Conn); ok {
		v := counter.perIPTLSConnPool.Get()
		if v == nil {
			return &perIPTLSConn{
				perIPConnCounter: counter,
				Conn:             tlsConn,
				ip:               ip,
			}
		}
		c := v.(*perIPTLSConn)
		c.Conn = tlsConn
		c.ip = ip
		return c
	}

	v := counter.perIPConnPool.Get()
	if v == nil {
		return &perIPConn{
			perIPConnCounter: counter,
			Conn:             conn,
			ip:               ip,
		}
	}
	c := v.(*perIPConn)
	c.Conn = conn
	c.ip = ip
	return c
}

func (c *perIPConn) Close() error {
	err := c.Conn.Close()
	c.perIPConnCounter.Unregister(c.ip)
	c.Conn = nil
	c.perIPConnCounter.perIPConnPool.Put(c)
	return err
}

func (c *perIPTLSConn) Close() error {
	err := c.Conn.Close()
	c.perIPConnCounter.Unregister(c.ip)
	c.Conn = nil
	c.perIPConnCounter.perIPTLSConnPool.Put(c)
	return err
}

func getUint32IP(c net.Conn) uint32 {
	return ip2uint32(getConnIP4(c))
}

func getConnIP4(c net.Conn) net.IP {
	addr := c.RemoteAddr()
	ipAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return net.IPv4zero
	}
	return ipAddr.IP.To4()
}

func ip2uint32(ip net.IP) uint32 {
	if len(ip) != 4 {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint322ip(ip uint32) net.IP {
	b := make([]byte, 4)
	b[0] = byte(ip >> 24)
	b[1] = byte(ip >> 16)
	b[2] = byte(ip >> 8)
	b[3] = byte(ip)
	return b
}
