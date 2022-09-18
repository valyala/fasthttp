package fasthttp

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type slowlorisCheckCounters struct {
	bytesTx              uint64
	prevBytesTx          uint64
	avgRate              float32
	txRateHistory        [4]float32
	lastTime             time.Time
	lowestThroughputKbps float32
	isMonitoring         int32
	isUnderLimit         int32
}
type slowlorisCheck struct {
	net.Conn
	r slowlorisCheckCounters
	w slowlorisCheckCounters
	pool *sync.Pool
}

func wrapSlowlorisCheck(s *Server, c net.Conn, lowestReadKbps float32, lowestWriteKbps float32) net.Conn {
	v := s.slowlorisCheckPool.Get()
	if v == nil {
		sc := &slowlorisCheck{
			Conn: c,
			pool: &s.slowlorisCheckPool,
		}
		sc.r.lowestThroughputKbps = lowestReadKbps
		sc.w.lowestThroughputKbps = lowestWriteKbps
		return sc
	}
	sc := v.(*slowlorisCheck)
	sc.Conn = c
	sc.pool = &s.slowlorisCheckPool
	sc.r.lowestThroughputKbps = lowestReadKbps
	sc.w.lowestThroughputKbps = lowestWriteKbps
	return sc
}

func releaseSlowlorisCheck(sc *slowlorisCheck) {
	pool := sc.pool
	sc.pool = nil
	sc.Conn = nil
	sc.r = slowlorisCheckCounters{}
	sc.w = slowlorisCheckCounters{}
	pool.Put(sc)
}

func (sc *slowlorisCheck) Close() error {
	err := sc.Conn.Close()
	releaseSlowlorisCheck(sc)
	return err
}

func (sc *slowlorisCheck) Read(b []byte) (n int, err error) {
	n, err = sc.Conn.Read(b)
	if n > 0 {
		sc.update(n, false)
	}
	return
}

func (sc *slowlorisCheck) Write(b []byte) (n int, err error) {
	n, err = sc.Conn.Write(b)
	if n > 0 {
		sc.update(n, true)
	}
	return
}

func (sc *slowlorisCheck) Monitor(write bool) (stop chan struct{}) {
	cnt := &sc.r
	if write {
		cnt = &sc.w
	}

	stop = make(chan struct{})

	if cnt.lowestThroughputKbps > 0.0 {
		cnt.lastTime = time.Now()
		atomic.StoreInt32(&cnt.isUnderLimit, 0)
		atomic.StoreInt32(&cnt.isMonitoring, 1)
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			for {
				select {
				case <-t.C:
					if atomic.LoadInt32(&cnt.isUnderLimit) != 0 {
						_ = sc.Conn.Close()
						t.Stop()
						atomic.StoreInt32(&cnt.isMonitoring, 0)
						return
					}

				case <-stop:
					t.Stop()
					atomic.StoreInt32(&cnt.isMonitoring, 0)
					return
				}
			}
		}()
	}
	return stop
}

func (sc *slowlorisCheck) update(bytesTx int, write bool) {
	cnt := &sc.r
	if write {
		cnt = &sc.w
	}

	if atomic.LoadInt32(&cnt.isMonitoring) != 0 {
		cnt.bytesTx += uint64(uint(bytesTx))

		now := time.Now()
		diffTime := now.Sub(cnt.lastTime)
		if diffTime >= 500*time.Millisecond {
			cnt.lastTime = now
			diffTx := cnt.bytesTx - cnt.prevBytesTx
			cnt.txRateHistory[3] = cnt.txRateHistory[2]
			cnt.txRateHistory[2] = cnt.txRateHistory[1]
			cnt.txRateHistory[1] = cnt.txRateHistory[0]
			cnt.txRateHistory[0] = float32(diffTx) / (float32(diffTime) * 1.024)

			count := 0
			var avgRate float32
			for _, txRateHist := range cnt.txRateHistory {
				if txRateHist > 0.000001 {
					avgRate += 1.0 / txRateHist
					count += 1
				}
			}

			cnt.prevBytesTx = cnt.bytesTx

			cnt.avgRate = float32(count) / avgRate

			if cnt.avgRate < cnt.lowestThroughputKbps {
				atomic.StoreInt32(&cnt.isUnderLimit, 1)
			} else {
				atomic.StoreInt32(&cnt.isUnderLimit, 0)
			}
		}
	}
}
