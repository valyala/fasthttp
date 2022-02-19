package pool

import (
	"sync"
	"sync/atomic"
	"time"
)

// LIFO pool, i.e. the most recently Put() item will return in call Get()
// Such a scheme keeps CPU caches hot (in theory).
// 
// Due to LIFO behaviors and MaxItems logic can't implement with sync/atomic like sync.Pool
type LIFO struct {
	MaxItems    int64
	MinItems    int64
	MaxIdles    int64
	IdleTimeout time.Duration
	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	New func() interface{}
	// Close optionally specifies a function to call when an item want to drop after timeout
	Close func(item interface{})

	itemsCount      int64 // idles + actives
	idleItemsLen    int64
	activeIdleCount int64
	cleanCount      int64
	mutex           sync.Mutex // lock below fields until new line
	idleItems       []interface{}
	head            int64
	tail            int64
	victimItems     []interface{}

	state int32
}

func (p *LIFO) State() PoolStatus         { return PoolStatus(atomic.LoadInt32(&p.state)) }
func (p *LIFO) SetState(state PoolStatus) { atomic.StoreInt32(&p.state, int32(state)) }
func (p *LIFO) Len() int                  { return int(atomic.LoadInt64(&p.idleItemsLen)) }

func (p *LIFO) Get() (item interface{}) {
	if p.isStop() {
		return
	}

	item = p.popHead()
	if item == nil && p.New != nil {
		item = p.makeNew()
	}
	return
}

func (p *LIFO) Put(item interface{}) {
	if p.isStop() {
		p.Close(item)
		return
	}

	p.pushHead(item)
}

func (p *LIFO) Start() {
	var pStatus = p.State()
	if pStatus != PoolStatus_Unset {
		if pStatus == PoolStatus_Running {
			panic("BUG: Pool already started")
		}
		if pStatus == PoolStatus_Stopping {
			panic("BUG: Pool is on stopping state and can't re-start before last stop proccess")
		}
		// Let pool to reuse in PoolStatus_Stopped state.
	}
	p.SetState(PoolStatus_Running)
	p.init()
	go p.Clean()
}

func (p *LIFO) Stop() {
	var wpStatus = p.State()
	if wpStatus != PoolStatus_Running {
		panic("BUG: pool wasn't started")
	}

	p.SetState(PoolStatus_Stopping)

	for i := 0; i < len(p.idleItems); i++ {
		var item = p.idleItems[i]
		if item != nil {
			p.idleItems[i] = nil
			p.Close(item)
		}
	}
	p.head, p.tail = 0, 0
	p.itemsCount = 0
	p.idleItemsLen = 0
	p.activeIdleCount = 0
	p.cleanCount = 0

	p.SetState(PoolStatus_Stopped)
}

// Clean items but not by exact p.IdleTimeout. To improve performance we choose two window clean up.
// Means some idle items can live up to double of p.IdleTimeout
func (p *LIFO) Clean() {
	for {
		time.Sleep(p.IdleTimeout)
		if p.isStop() {
			break
		}

		var aic = atomic.SwapInt64(&p.activeIdleCount, 0)
		var iil = atomic.LoadInt64(&p.idleItemsLen)
		if iil < p.MinItems || p.cleanCount < 1 {
			p.cleanCount = aic
			continue
		}
		if aic < 0 {
			p.cleanCount -= +aic
			if p.cleanCount < 1 {
				continue
			}
		}

		p.popsTail(p.cleanCount)
		// Close items outside of p.mutex
		var vi = p.victimItems
		for i := 0; i < len(vi); i++ {
			vi[i] = nil
			p.Close(vi[i])
		}
	}
}

func (p *LIFO) init() {
	if p.Close == nil {
		p.Close = func(item interface{}) {}
	}
	if len(p.idleItems) == 0 {
		p.idleItems = make([]interface{}, p.MaxItems)
	}
}

func (p *LIFO) isStop() bool {
	var wpStatus = p.State()
	if wpStatus == PoolStatus_Stopping || wpStatus == PoolStatus_Stopped {
		return true
	}
	return false
}

func (p *LIFO) popHead() (item interface{}) {
	p.mutex.Lock()
	// check queue is not empty.
	if p.tail != p.head {
		p.head--
		item = p.idleItems[p.head]
	}
	p.mutex.Unlock()

	atomic.AddInt64(&p.idleItemsLen, -1)
	atomic.AddInt64(&p.activeIdleCount, -1)
	return
}

func (p *LIFO) pushHead(item interface{}) {
	p.mutex.Lock()
	if int64(len(p.idleItems)) == p.head {
		p.idleItems[0] = item
		p.head = 1
	} else {
		p.idleItems[p.head] = item
		p.head++
	}
	p.mutex.Unlock()

	atomic.AddInt64(&p.idleItemsLen, 1)
	atomic.AddInt64(&p.activeIdleCount, 1)
}

func (p *LIFO) popsTail(num int64) {
	p.mutex.Lock()
	// check queue is not empty.
	if p.tail != p.head {
		var end = p.tail + num
		var tail []interface{}
		if end > int64(len(p.idleItems)) {
			tail = p.idleItems[p.tail:]
			p.victimItems = append(p.victimItems[:0], tail...)
			for i := 0; i < len(tail); i++ {
				tail[i] = nil
			}

			end = num - int64(len(p.victimItems))
			tail = p.idleItems[:end]
			p.victimItems = append(p.victimItems, tail...)
			for i := 0; i < len(tail); i++ {
				tail[i] = nil
			}
		} else {
			tail = p.idleItems[p.tail:end]
			p.victimItems = append(p.victimItems[:0], tail...)
			for i := 0; i < len(tail); i++ {
				tail[i] = nil
			}
		}
	}
	p.mutex.Unlock()
}

func (p *LIFO) makeNew() (item interface{}) {
	var ic = atomic.AddInt64(&p.itemsCount, 1)
	if ic <= p.MaxItems {
		item = p.New()
	} else {
		atomic.AddInt64(&p.itemsCount, -1)
	}
	return
}
