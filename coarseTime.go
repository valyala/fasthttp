package fasthttp

import (
	"sync/atomic"
	"time"
)

func coarseTimeNow() time.Time {
	tp := coarseTime.Load().(*time.Time)
	return *tp
}

func init() {
	t := time.Now()
	coarseTime.Store(&t)
	go func() {
		for {
			time.Sleep(time.Second)
			t := time.Now()
			coarseTime.Store(&t)
		}
	}()
}

var coarseTime atomic.Value
