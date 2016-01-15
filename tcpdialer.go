package fasthttp

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	dial          = (&tcpDialer{}).NewDial()
	dialDualStack = (&tcpDialer{DualStack: true}).NewDial()
)

// Dial dials the given TCP addr using tcp4.
//
// This function has the following additional features comparing to net.Dial:
//
//   * It reduces load on DNS resolver by caching resolved TCP addressed
//     for one minute.
//   * It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//   * It returns ErrDialTimeout if connection cannot be established during
//     DefaultDialTimeout seconds.
//
// This dialer is intended for custom code wrapping before passing
// to Client.Dial or HostClient.Dial.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//     * foobar.baz:443
//     * foo.bar:80
//     * aaa.com:8080
func Dial(addr string) (net.Conn, error) {
	return dial(addr)
}

// DialDualStack dials the given TCP addr using both tcp4 and tcp6.
//
// This function has the following additional features comparing to net.Dial:
//
//   * It reduces load on DNS resolver by caching resolved TCP addressed
//     for one minute.
//   * It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//   * It returns ErrDialTimeout if connection cannot be established during
//     DefaultDialTimeout seconds.
//
// This dialer is intended for custom code wrapping before passing
// to Client.Dial or HostClient.Dial.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//     * foobar.baz:443
//     * foo.bar:80
//     * aaa.com:8080
func DialDualStack(addr string) (net.Conn, error) {
	return dialDualStack(addr)
}

type tcpDialer struct {
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
		addrs, idx, err := d.getTCPAddrs(addr)
		if err != nil {
			return nil, err
		}
		network := "tcp4"
		if d.DualStack {
			network = "tcp"
		}

		var conn net.Conn
		startTime := time.Now()
		n := uint32(len(addrs))
		for n > 0 {
			conn, err = tryDial(network, &addrs[idx%n])
			if err == nil {
				return conn, nil
			}
			if time.Since(startTime) > DefaultDialTimeout {
				return nil, ErrDialTimeout
			}
			idx++
			n--
		}
		return nil, err
	}
}

func tryDial(network string, addr *net.TCPAddr) (net.Conn, error) {
	ch := make(chan dialResult, 1)
	go func() {
		var dr dialResult
		dr.conn, dr.err = net.DialTCP(network, nil, addr)
		ch <- dr
	}()
	select {
	case dr := <-ch:
		return dr.conn, dr.err
	case <-time.After(DefaultDialTimeout):
		return nil, ErrDialTimeout
	}
}

type dialResult struct {
	conn net.Conn
	err  error
}

// ErrDialTimeout is returned when TCP dialing is timed out.
var ErrDialTimeout = errors.New("dialing to the given TCP address timed out")

// DefaultDialTimeout is timeout used by Dial and DialDualStack
// for establishing TCP connections.
const DefaultDialTimeout = 10 * time.Second

type tcpAddrEntry struct {
	addrs    []net.TCPAddr
	addrsIdx uint32

	resolveTime time.Time
	pending     bool
}

const tcpAddrsCacheDuration = time.Minute

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

func (d *tcpDialer) getTCPAddrs(addr string) ([]net.TCPAddr, uint32, error) {
	d.tcpAddrsLock.Lock()
	e := d.tcpAddrsMap[addr]
	if e != nil && !e.pending && time.Since(e.resolveTime) > tcpAddrsCacheDuration {
		e.pending = true
		e = nil
	}
	d.tcpAddrsLock.Unlock()

	if e == nil {
		addrs, err := resolveTCPAddrs(addr, d.DualStack)
		if err != nil {
			d.tcpAddrsLock.Lock()
			e = d.tcpAddrsMap[addr]
			if e != nil && e.pending {
				e.pending = false
			}
			d.tcpAddrsLock.Unlock()
			return nil, 0, err
		}

		e = &tcpAddrEntry{
			addrs:       addrs,
			resolveTime: time.Now(),
		}

		d.tcpAddrsLock.Lock()
		d.tcpAddrsMap[addr] = e
		d.tcpAddrsLock.Unlock()
	}

	idx := uint32(0)
	if len(e.addrs) > 0 {
		idx = atomic.AddUint32(&e.addrsIdx, 1)
	}
	return e.addrs, idx, nil
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
