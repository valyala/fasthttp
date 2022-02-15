package fasthttp

import (
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// workerPool serves incoming connections via a pool of workers
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

	workersCount         int64
	idleWorkers          sync.Pool
	lastIdleWorkersCount int64
	idleWorkersCount     int64
	state                int32
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
	if wp.MaxIdleWorkerDuration <= 0 {
		wp.MaxIdleWorkerDuration = 10 * time.Second
	}
	wp.SetState(workerPoolState_Running)

	go func() {
		for {
			time.Sleep(wp.MaxIdleWorkerDuration)
			if wp.isStop() {
				break
			}
			wp.clean()
		}
	}()
}

func (wp *workerPool) Stop() {
	var wpStatus = wp.State()
	if wpStatus != workerPoolState_Running {
		panic("BUG: workerPool wasn't started")
	}

	// Do not wait for busy workers - they will stop after
	// serving the connection and noticing the stopping state.
	wp.SetState(workerPoolState_Stopping)

	// Stop all the workers waiting for incoming connections.
	var wc = atomic.LoadInt64(&wp.workersCount)
	for i := int64(0); i < wc; i++ {
		var w = wp.idleWorkers.Get()
		if w == nil {
			break
		}
		w.(*workerChan).ch <- nil
	}

	wp.SetState(workerPoolState_Stopped)
}

func (wp *workerPool) Serve(c net.Conn) bool {
	var w = wp.getWorker()
	if w == nil {
		return false
	}
	w.ch <- c
	return true
}

func (wp *workerPool) isStop() bool {
	var wpStatus = wp.State()
	if wpStatus == workerPoolState_Stopping || wpStatus == workerPoolState_Stopped {
		return true
	}
	return false
}

func (wp *workerPool) clean() {
	var iwc = atomic.SwapInt64(&wp.idleWorkersCount, 0)
	var liwc = atomic.SwapInt64(&wp.lastIdleWorkersCount, iwc)
	if (iwc < wp.MaxWorkersCount*10/100) || // Don't clean if idle worker numbers smaller than 10% of allowed workers numbers
		iwc < 0 || // Don't clean when active idle worker numbers is negative
		iwc < liwc { // Don't clean when active idle worker numbers period smaller than last one
		return
	}

	var cleanNumber = iwc - liwc
	// Notify obsolete workers to stop.
	// This notification must be outside the wp.lock, since ch.ch
	// may be blocking and may consume a lot of time if many workers
	// are located on non-local CPUs.
	for i := int64(0); i < cleanNumber; i++ {
		var w = wp.idleWorkers.Get()
		if w == nil {
			continue
		}
		w.(*workerChan).ch <- nil
	}
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

func (wp *workerPool) getWorker() (ch *workerChan) {
	ch = wp.getIdleWorker()
	if ch == nil {
		ch = wp.makeNewWorker()
	}
	return
}

func (wp *workerPool) getIdleWorker() (ch *workerChan) {
	var w = wp.idleWorkers.Get()
	if w != nil {
		ch = w.(*workerChan)
		atomic.AddInt64(&wp.idleWorkersCount, -1)
	}
	return
}

func (wp *workerPool) makeNewWorker() (ch *workerChan) {
	var wc = atomic.LoadInt64(&wp.workersCount)
	if wc < wp.MaxWorkersCount {
		ch = &workerChan{
			ch: make(chan net.Conn, workerChanCap),
		}
		atomic.AddInt64(&wp.workersCount, 1)

		go wp.workerFunc(ch)
	}
	return
}

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

		if wp.isStop() {
			break
		}

		wp.idleWorkers.Put(ch)
		atomic.AddInt64(&wp.idleWorkersCount, 1)
	}

	atomic.AddInt64(&wp.workersCount, -1)
}
