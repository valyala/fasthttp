package fasthttp

import (
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp/pool"
)

// workerPool serves incoming connections via a pool of workers
// in LIFO order, i.e. the most recently stopped worker will serve the next
// incoming connection.
//
// Such a scheme keeps CPU caches hot (in theory).
type workerPool struct {
	// Function for serving server connections.
	// It must leave c unclosed.
	WorkerFunc            ServeHandler
	MaxWorkersCount       int64
	LogAllErrors          bool
	MaxIdleWorkerDuration time.Duration
	Logger                Logger
	ConnState             func(net.Conn, ConnState)

	idleWorkers pool.LIFO
	state       int32
}

type workerPoolState int32

const (
	workerPoolState_Unset workerPoolState = iota
	workerPoolState_Running
	workerPoolState_Stopping
	workerPoolState_Stopped
)

type workerChan struct {
	ch chan net.Conn
}

func (wp *workerPool) State() workerPoolState         { return workerPoolState(atomic.LoadInt32(&wp.state)) }
func (wp *workerPool) SetState(state workerPoolState) { atomic.StoreInt32(&wp.state, int32(state)) }

func (wp *workerPool) Start() {
	var wpStatus = wp.State()
	if wpStatus != workerPoolState_Unset {
		if wpStatus == workerPoolState_Running {
			panic("BUG: workerPool already started")
		}
		if wpStatus == workerPoolState_Stopping {
			panic("BUG: workerPool is on stopping state and can't re-start before last stop proccess")
		}
		// Let worker pool to reuse in workerPoolState_Stopped state.
	}
	wp.SetState(workerPoolState_Running)

	if wp.MaxIdleWorkerDuration <= 0 {
		wp.MaxIdleWorkerDuration = 10 * time.Second
	}

	wp.idleWorkers.MaxItems = wp.MaxWorkersCount
	// wp.idleWorkers.MinItems = wp.MaxWorkersCount / 8
	wp.idleWorkers.IdleTimeout = wp.MaxIdleWorkerDuration
	wp.idleWorkers.New = func() interface{} {
		var ch = &workerChan{
			ch: make(chan net.Conn, workerChanCap),
		}
		go wp.workerFunc(ch)
		return ch
	}
	wp.idleWorkers.Close = func(item interface{}) {
		item.(*workerChan).ch <- nil
	}
	wp.idleWorkers.Start()
}

func (wp *workerPool) Stop() {
	var wpStatus = wp.State()
	if wpStatus != workerPoolState_Running {
		panic("BUG: workerPool wasn't started")
	}

	wp.SetState(workerPoolState_Stopping)
	wp.idleWorkers.Stop()
	wp.SetState(workerPoolState_Stopped)
}

func (wp *workerPool) Serve(c net.Conn) bool {
	var w = wp.idleWorkers.Get()
	if w == nil {
		return false
	}
	w.(*workerChan).ch <- c
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

func (wp *workerPool) workerFunc(ch *workerChan) {
	for c := range ch.ch {
		if c == nil {
			break
		}

		var err = wp.WorkerFunc(c)
		if err != nil && err != errHijacked {
			errStr := err.Error()
			if wp.LogAllErrors || !(strings.Contains(errStr, "broken pipe") ||
				strings.Contains(errStr, "reset by peer") ||
				strings.Contains(errStr, "request headers: small read buffer") ||
				strings.Contains(errStr, "unexpected EOF") ||
				strings.Contains(errStr, "i/o timeout")) {
				wp.Logger.Printf("error when serving connection %q<->%q: %s", c.LocalAddr(), c.RemoteAddr(), err)
			}
		}
		if err == errHijacked {
			wp.ConnState(c, StateHijacked)
		} else {
			_ = c.Close()
			wp.ConnState(c, StateClosed)
		}

		wp.idleWorkers.Put(ch)
	}
}
