package fasthttp

import (
	"sync"
	"sync/atomic"
	"time"
)

type serverDateUpdater struct {
	mtx        sync.Mutex
	useCounter int32
	date       atomic.Value
	stopCh     chan struct{}

	zeroLenBuffer   []byte

	slowPathBufferMtx sync.Mutex
	slowPathBuffer   []byte
	slowPathLastTime time.Time
}

var (
	serverDateUpdaterData = serverDateUpdater{
		useCounter:        0,

		zeroLenBuffer:     make([]byte, 0),

		slowPathBuffer:    make([]byte, 0),
		slowPathBufferMtx: sync.Mutex{},
		slowPathLastTime:  time.Now().AddDate(0, 0, -1),
	}
)

// NOTE: Ensure one call to startServerDateUpdater matches always one call to stopServerDateUpdater
func startServerDateUpdater() {
	serverDateUpdaterData.mtx.Lock()
	defer serverDateUpdaterData.mtx.Unlock()

	serverDateUpdaterData.useCounter += 1
	if serverDateUpdaterData.useCounter == 1 {
		serverDateUpdaterData.stopCh = make(chan struct{})
		go updateServerDate()
	}
}

func stopServerDateUpdater() {
	serverDateUpdaterData.mtx.Lock()
	defer serverDateUpdaterData.mtx.Unlock()

	serverDateUpdaterData.useCounter -= 1
	if serverDateUpdaterData.useCounter == 0 {
		close(serverDateUpdaterData.stopCh)
		serverDateUpdaterData.date.Store(serverDateUpdaterData.zeroLenBuffer) // Store a dummy non-nil value
	}
}

func updateServerDate() {
	refreshServerDate()
	go func() {
		for {
			select {
			case <-serverDateUpdaterData.stopCh:
				return

			case <-time.After(time.Second):
				refreshServerDate()
			}
		}
	}()
}


func refreshServerDate() {
	b := AppendHTTPDate(nil, time.Now())
	serverDateUpdaterData.date.Store(b)
}

func getServerDate() []byte {
	b, ok := serverDateUpdaterData.date.Load().([]byte)
	if !ok || len(b) == 0 {
		// Slow path, mostly used when requests are manually served by ServeConn
		serverDateUpdaterData.mtx.Lock()
		defer serverDateUpdaterData.mtx.Unlock()

		now := time.Now()
		if now.After(serverDateUpdaterData.slowPathLastTime) {
			serverDateUpdaterData.slowPathLastTime = now.Add(time.Second)
			serverDateUpdaterData.slowPathBuffer = AppendHTTPDate(nil, now)
		}
		return serverDateUpdaterData.slowPathBuffer
	}
	return b
}
