package fasthttp

import (
	"net"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// workerPool serves incoming connections via a pool of workers
// in FILO order, i.e. the most recently stopped worker will serve the next
// incoming connection.
//
// Such a scheme keeps CPU caches hot (in theory).
type workerPool struct {
	// Function for serving server connections.
	// It must leave c unclosed.
	WorkerFunc func(c net.Conn) error

	// Maximum number of workers to create.
	MaxWorkersCount int

	// Logger used by workerPool.
	Logger Logger

	lock         sync.Mutex
	workersCount int
	mustStop     bool

	ready []*workerChan

	stopCh chan struct{}
}

type workerChan struct {
	t  time.Time
	ch chan net.Conn
}

func (wp *workerPool) Start() {
	if wp.stopCh != nil {
		panic("BUG: workerPool already started")
	}
	wp.stopCh = make(chan struct{})
	stopCh := wp.stopCh
	go func() {
		for {
			select {
			case <-stopCh:
				return
			default:
				time.Sleep(10 * time.Second)
			}
			wp.clean()
		}
	}()
}

func (wp *workerPool) Stop() {
	if wp.stopCh == nil {
		panic("BUG: workerPool wasn't started")
	}
	close(wp.stopCh)
	wp.stopCh = nil

	// Stop all the workers waiting for incoming connections.
	// Do not wait for busy workers - they will stop after
	// serving the connection and noticing wp.mustStop = true.
	wp.lock.Lock()
	for _, ch := range wp.ready {
		ch.ch <- nil
	}
	wp.ready = nil
	wp.mustStop = true
	wp.lock.Unlock()
}

func (wp *workerPool) clean() {
	// Clean least recently used workers if they didn't serve connections
	// for more than one second.
	wp.lock.Lock()
	ready := wp.ready
	for len(ready) > 1 && time.Since(ready[0].t) > 10*time.Second {
		// notify the worker to stop.
		ready[0].ch <- nil

		ready = ready[1:]
		wp.workersCount--
	}
	if len(ready) < len(wp.ready) {
		copy(wp.ready, ready)
		for i := len(ready); i < len(wp.ready); i++ {
			wp.ready[i] = nil
		}
		wp.ready = wp.ready[:len(ready)]
	}
	wp.lock.Unlock()
}

func (wp *workerPool) Serve(c net.Conn) bool {
	ch := wp.getCh()
	if ch == nil {
		return false
	}
	ch.ch <- c
	return true
}

var workerChanCap = func() int {
	// Use blocking workerChan if GOMAXPROCS=1.
	// This immediately switches Serve to WorkerFunc, which results
	// in higher performance (under go1.5 at least).
	if runtime.GOMAXPROCS(0) == 1 {
		return 0
	}

	// Use non-blocking workerChan if GOMAXPROCS>1,
	// since otherwise the Serve caller (Acceptor) may lag accepting
	// new connections if WorkerFunc is CPU-bound.
	return 1
}()

func (wp *workerPool) getCh() *workerChan {
	var ch *workerChan
	createWorker := false

	wp.lock.Lock()
	ready := wp.ready
	n := len(ready) - 1
	if n < 0 {
		if wp.workersCount < wp.MaxWorkersCount {
			createWorker = true
			wp.workersCount++
		}
	} else {
		ch = ready[n]
		wp.ready = ready[:n]
	}
	wp.lock.Unlock()

	if ch == nil {
		if !createWorker {
			return nil
		}
		vch := workerChanPool.Get()
		if vch == nil {
			vch = &workerChan{
				ch: make(chan net.Conn, workerChanCap),
			}
		}
		ch = vch.(*workerChan)
		go func() {
			wp.workerFunc(ch)
			workerChanPool.Put(vch)
		}()
	}
	return ch
}

func (wp *workerPool) release(ch *workerChan) bool {
	ch.t = time.Now()
	wp.lock.Lock()
	if wp.mustStop {
		wp.lock.Unlock()
		return false
	}
	wp.ready = append(wp.ready, ch)
	wp.lock.Unlock()
	return true
}

var workerChanPool sync.Pool

func (wp *workerPool) workerFunc(ch *workerChan) {
	var c net.Conn
	var err error

	defer func() {
		if r := recover(); r != nil {
			wp.Logger.Printf("panic: %s\nStack trace:\n%s", r, debug.Stack())
		}

		if c != nil {
			c.Close()
			wp.release(ch)
		}
	}()

	for c = range ch.ch {
		if c == nil {
			break
		}
		if err = wp.WorkerFunc(c); err != nil && err != errHijacked {
			errStr := err.Error()
			if !strings.Contains(errStr, "broken pipe") && !strings.Contains(errStr, "reset by peer") {
				wp.Logger.Printf("error when serving connection %q<->%q: %s", c.LocalAddr(), c.RemoteAddr(), err)
			}
		}
		if err != errHijacked {
			c.Close()
		}
		c = nil

		if !wp.release(ch) {
			break
		}
	}
}
