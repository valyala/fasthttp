package fasthttp

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// BalancingClient is the interface for clients, which may be passed
// to LBClient.Clients.
type BalancingClient interface {
	DoDeadline(req *Request, resp *Response, deadline time.Time) error
	PendingRequests() int
}

// LBClient balances requests among available LBClient.Clients.
//
// It has the following features:
//
//   - Balances load among available clients using 'least loaded' + 'least total'
//     hybrid technique.
//   - Dynamically decreases load on unhealthy clients.
//
// It is forbidden copying LBClient instances. Create new instances instead.
//
// It is safe calling LBClient methods from concurrently running goroutines.
type LBClient struct {
	noCopy noCopy //nolint:unused,structcheck

	// Clients must contain non-zero clients list.
	// Incoming requests are balanced among these clients.
	Clients []BalancingClient

	// HealthCheck is a callback called after each request.
	//
	// The request, response and the error returned by the client
	// is passed to HealthCheck, so the callback may determine whether
	// the client is healthy.
	//
	// Load on the current client is decreased if HealthCheck returns false.
	//
	// By default HealthCheck returns false if err != nil.
	HealthCheck func(req *Request, resp *Response, err error) bool

	// Timeout is the request timeout used when calling LBClient.Do.
	//
	// DefaultLBClientTimeout is used by default.
	Timeout time.Duration

	cs []*lbClient

	once sync.Once
}

// DefaultLBClientTimeout is the default request timeout used by LBClient
// when calling LBClient.Do.
//
// The timeout may be overridden via LBClient.Timeout.
const DefaultLBClientTimeout = time.Second

var ErrNoAliveClients = errors.New("no alive clients available")

// DoDeadline calls DoDeadline on the least loaded client
func (cc *LBClient) DoDeadline(req *Request, resp *Response, deadline time.Time) error {
	c := cc.get()
	if c == nil {
		return ErrNoAliveClients
	}
	return c.DoDeadline(req, resp, deadline)
}

// DoTimeout calculates deadline and calls DoDeadline on the least loaded client
func (cc *LBClient) DoTimeout(req *Request, resp *Response, timeout time.Duration) error {
	c := cc.get()
	if c == nil {
		return ErrNoAliveClients
	}
	deadline := time.Now().Add(timeout)
	return c.DoDeadline(req, resp, deadline)
}

// Do calls calculates deadline using LBClient.Timeout and calls DoDeadline
// on the least loaded client.
func (cc *LBClient) Do(req *Request, resp *Response) error {
	timeout := cc.Timeout
	if timeout <= 0 {
		timeout = DefaultLBClientTimeout
	}
	return cc.DoTimeout(req, resp, timeout)
}

func (cc *LBClient) init() {
	if len(cc.Clients) == 0 {
		panic("BUG: LBClient.Clients cannot be empty")
	}
	for _, c := range cc.Clients {
		cc.cs = append(cc.cs, &lbClient{
			c:           c,
			healthCheck: cc.HealthCheck,
		})
	}
}

func (cc *LBClient) get() *lbClient {
	cc.once.Do(cc.init)

	cs := cc.cs
	var (
		minC *lbClient
		off  int
	)
	for i, c := range cs {
		if !c.underPenalty() {
			minC = c
			off = i + 1
			break
		}
	}
	if minC == nil {
		return nil
	}

	if off < len(cs) {
		minN := minC.PendingRequests()
		minT := atomic.LoadUint64(&minC.total)
		for _, c := range cs[off:] {
			if c.underPenalty() {
				continue
			}
			n := c.PendingRequests()
			t := atomic.LoadUint64(&c.total)
			if n < minN || (n == minN && t < minT) {
				minC = c
				minN = n
				minT = t
			}
		}
	}
	return minC
}

type lbClient struct {
	c           BalancingClient
	healthCheck func(req *Request, resp *Response, err error) bool
	penalty     uint32

	// total amount of requests handled.
	total uint64
}

func (c *lbClient) DoDeadline(req *Request, resp *Response, deadline time.Time) error {
	err := c.c.DoDeadline(req, resp, deadline)
	if !c.isHealthy(req, resp, err) && c.incPenalty() {
		// Penalize the client returning error, so the next requests
		// are routed to another clients.
		time.AfterFunc(penaltyDuration, c.decPenalty)
	} else {
		atomic.AddUint64(&c.total, 1)
	}
	return err
}

func (c *lbClient) PendingRequests() int {
	n := c.c.PendingRequests()
	m := atomic.LoadUint32(&c.penalty)
	return n + int(m)
}

func (c *lbClient) isHealthy(req *Request, resp *Response, err error) bool {
	if c.healthCheck == nil {
		return err == nil
	}
	return c.healthCheck(req, resp, err)
}

func (c *lbClient) underPenalty() bool {
	return atomic.LoadUint32(&c.penalty) > 0
}

func (c *lbClient) incPenalty() bool {
	m := atomic.AddUint32(&c.penalty, 1)
	if m > maxPenalty {
		c.decPenalty()
		return false
	}
	return true
}

func (c *lbClient) decPenalty() {
	atomic.AddUint32(&c.penalty, ^uint32(0))
}

const (
	maxPenalty = 300

	penaltyDuration = 3 * time.Second
)
