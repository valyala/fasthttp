package proxy

import "sync"

type perIPConnCounter struct {
	pool sync.Pool
	lock sync.Mutex
	m    map[uint32]int
}
