package fasthttp

import (
	"net"
	"runtime/debug"
	"sync"
	"time"
)

// workerPool serves incoming connections via a pool of workers
// in FIFO order, i.e. the most recently stopped worker will serve the next
// incoming connection.
type workerPool struct {
	// Function for serving server connections.
	// It must close c before returning.
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
				time.Sleep(time.Second)
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
	chans := wp.ready
	for len(chans) > 1 && time.Since(chans[0].t) > time.Second {
		chans[0].ch <- nil
		copy(chans, chans[1:])
		chans = chans[:len(chans)-1]
		wp.ready = chans
		wp.workersCount--
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

func (wp *workerPool) getCh() *workerChan {
	var ch *workerChan
	createWorker := false

	wp.lock.Lock()
	chans := wp.ready
	n := len(chans) - 1
	if n < 0 {
		if wp.workersCount < wp.MaxWorkersCount {
			createWorker = true
			wp.workersCount++
		}
	} else {
		ch = chans[n]
		wp.ready = chans[:n]
	}
	wp.lock.Unlock()

	if ch == nil {
		if !createWorker {
			return nil
		}
		vch := workerChanPool.Get()
		if vch == nil {
			vch = &workerChan{
				ch: make(chan net.Conn, 1),
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
		return false
	}
	wp.ready = append(wp.ready, ch)
	wp.lock.Unlock()
	return true
}

var workerChanPool sync.Pool

func (wp *workerPool) workerFunc(ch *workerChan) {
	defer func() {
		if r := recover(); r != nil {
			wp.Logger.Printf("panic: %s\nStack trace:\n%s", r, debug.Stack())
		}
	}()

	var c net.Conn
	var err error
	for c = range ch.ch {
		if c == nil {
			break
		}
		if err = wp.WorkerFunc(c); err != nil {
			wp.Logger.Printf("error when serving connection %q<->%q: %s", c.LocalAddr(), c.RemoteAddr(), err)
		}
		c = nil

		if !wp.release(ch) {
			break
		}
	}
}
