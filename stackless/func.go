package stackless

import (
	"runtime"
	"sync"
)

// NewFunc returns stackless wrapper for the function f.
//
// Unlike f, the returned stackless wrapper doesn't use stack space
// on the goroutine that calls it.
// The wrapper may save a lot of stack space if the following conditions
// are met:
//
//     - f doesn't contain blocking calls on network, I/O or channels;
//     - f uses a lot of stack space;
//     - the wrapper is called from high number of concurrent goroutines.
//
// The stackless wrapper returns false if the call cannot be processed
// at the moment due to high load.
func NewFunc(f func(ctx interface{})) func(ctx interface{}) bool {
	if f == nil {
		panic("BUG: f cannot be nil")
	}
	return func(ctx interface{}) bool {
		fw := getFuncWork()
		fw.f = f
		fw.ctx = ctx

		select {
		case funcWorkCh <- fw:
		default:
			putFuncWork(fw)
			return false
		}
		<-fw.done
		putFuncWork(fw)
		return true
	}
}

func init() {
	n := runtime.GOMAXPROCS(-1)
	for i := 0; i < n; i++ {
		go funcWorker()
	}
}

func funcWorker() {
	for fw := range funcWorkCh {
		fw.f(fw.ctx)
		fw.done <- struct{}{}
	}
}

var funcWorkCh = make(chan *funcWork, runtime.GOMAXPROCS(-1)*1024)

func getFuncWork() *funcWork {
	v := funcWorkPool.Get()
	if v == nil {
		v = &funcWork{
			done: make(chan struct{}, 1),
		}
	}
	return v.(*funcWork)
}

func putFuncWork(fw *funcWork) {
	fw.f = nil
	fw.ctx = nil
	funcWorkPool.Put(fw)
}

var funcWorkPool sync.Pool

type funcWork struct {
	f    func(ctx interface{})
	ctx  interface{}
	done chan struct{}
}
