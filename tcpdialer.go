package fasthttp

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TCP dialers used by client.
//
// These dialers are intended for custom code wrapping before passing
// to Client.Dial or HostClient.Dial.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to dial func may contain port. Example addr values:
//
//     * google.com
//     * foobar.baz:443
//     * foo.bar:80
//     * aaa.com:8080
//
// Default port is appended to the addr if port is missing:
//
//     * ':80' if Dial is used
//     * ':443' if DialTLS is used
var (
	// Dial dials the given addr using tcp4.
	// '80' port is used if port is missing in the addr passed to the func.
	Dial = DialFunc((&tcpDialer{}).NewDial())

	// DialTLS dials the given addr using tcp4.
	// '443' port is used if port is missing in the addr passed to the func.
	DialTLS = DialFunc((&tcpDialer{IsTLS: true}).NewDial())

	// DialDualStack dials the given addr using both tcp4 and tcp6.
	// '80' port is used if port is missing in the addr passed to the func.
	DialDualStack = DialFunc((&tcpDialer{DualStack: true}).NewDial())

	// DialTLSDualStack dials the given addr using both tcp4 and tcp6.
	// '443' port is used if port is missing in the addr passed to the func.
	DialTLSDualStack = DialFunc((&tcpDialer{IsTLS: true, DualStack: true}).NewDial())
)

// tcpDialer implements default TCP dialer for the Client and HostClient.
//
// tcpDialer instance copying is forbiddent. Create new instance instead.
type tcpDialer struct {
	// Appends ':80' to the addr with missing port in Dial if set to false.
	// Appends ':443' to the addr with missing port in Dial if set to true.
	IsTLS bool

	// Set to true if you want simultaneously dialing tcp4 and tcp6.
	DualStack bool

	tcpAddrsLock sync.Mutex
	tcpAddrsMap  map[string]*tcpAddrEntry
}

func (d *tcpDialer) NewDial() DialFunc {
	if d.tcpAddrsMap != nil {
		panic("BUG: NewDial() already called")
	}

	d.tcpAddrsMap = make(map[string]*tcpAddrEntry)
	go d.tcpAddrsClean()

	return func(addr string) (net.Conn, error) {
		tcpAddr, err := d.getTCPAddr(addr)
		if err != nil {
			return nil, err
		}
		network := "tcp4"
		if d.DualStack {
			network = "tcp"
		}
		return net.DialTCP(network, nil, tcpAddr)
	}
}

type tcpAddrEntry struct {
	addrs    []net.TCPAddr
	addrsIdx uint32

	resolveTime time.Time
	pending     bool
}

var tcpAddrsCacheDuration = time.Minute

func (d *tcpDialer) tcpAddrsClean() {
	expireDuration := 2 * tcpAddrsCacheDuration
	for {
		time.Sleep(time.Second)
		t := time.Now()

		d.tcpAddrsLock.Lock()
		for k, e := range d.tcpAddrsMap {
			if t.Sub(e.resolveTime) > expireDuration {
				delete(d.tcpAddrsMap, k)
			}
		}
		d.tcpAddrsLock.Unlock()
	}
}

func (d *tcpDialer) getTCPAddr(addr string) (*net.TCPAddr, error) {
	addr = addMissingPort(addr, d.IsTLS)

	d.tcpAddrsLock.Lock()
	e := d.tcpAddrsMap[addr]
	if e != nil && !e.pending && time.Since(e.resolveTime) > tcpAddrsCacheDuration {
		e.pending = true
		e = nil
	}
	d.tcpAddrsLock.Unlock()

	if e == nil {
		tcpAddrs, err := resolveTCPAddrs(addr, d.DualStack)
		if err != nil {
			d.tcpAddrsLock.Lock()
			e = d.tcpAddrsMap[addr]
			if e != nil && e.pending {
				e.pending = false
			}
			d.tcpAddrsLock.Unlock()
			return nil, err
		}

		e = &tcpAddrEntry{
			addrs:       tcpAddrs,
			resolveTime: time.Now(),
		}

		d.tcpAddrsLock.Lock()
		d.tcpAddrsMap[addr] = e
		d.tcpAddrsLock.Unlock()
	}

	tcpAddr := &e.addrs[0]
	n := len(e.addrs)
	if n > 1 {
		n := atomic.AddUint32(&e.addrsIdx, 1)
		tcpAddr = &e.addrs[n%uint32(n)]
	}
	return tcpAddr, nil
}

func addMissingPort(addr string, isTLS bool) string {
	n := strings.Index(addr, ":")
	if n >= 0 {
		return addr
	}
	port := 80
	if isTLS {
		port = 443
	}
	return fmt.Sprintf("%s:%d", addr, port)
}

func resolveTCPAddrs(addr string, dualStack bool) ([]net.TCPAddr, error) {
	host, portS, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portS)
	if err != nil {
		return nil, err
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	n := len(ips)
	addrs := make([]net.TCPAddr, 0, n)
	for i := 0; i < n; i++ {
		ip := ips[i]
		if !dualStack && ip.To4() == nil {
			continue
		}
		addrs = append(addrs, net.TCPAddr{
			IP:   ip,
			Port: port,
		})
	}
	return addrs, nil
}
