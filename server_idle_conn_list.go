package fasthttp

import (
	"net"
	"sync"
	"sync/atomic"
	"unsafe"
)

type idleConnList struct {
	mtx       sync.Mutex
	firstItem *idleConnListItem
	lastItem  *idleConnListItem
}

type idleConnListItem struct {
	nextItem *idleConnListItem
	prevItem *idleConnListItem
	c        net.Conn
	connTime atomic.Int64
}

func (l *idleConnList) insertBack(itemPtr uintptr) {
	item := (*idleConnListItem)(unsafe.Pointer(itemPtr))

	l.mtx.Lock()
	defer l.mtx.Unlock()

	if l.lastItem == nil {
		l.firstItem = item
		l.lastItem = item
	} else {
		l.lastItem.nextItem = item
		item.prevItem = l.lastItem
		l.lastItem = item
	}
}

func (l *idleConnList) remove(itemPtr uintptr) {
	l.mtx.Lock()
	defer l.mtx.Unlock()

	l.removeNoLock(itemPtr)
}

func (l *idleConnList) removeNoLock(itemPtr uintptr) {
	item := (*idleConnListItem)(unsafe.Pointer(itemPtr))

	if item.prevItem != nil {
		item.prevItem.nextItem = item.nextItem
	} else {
		l.firstItem = item.nextItem
	}
	if item.nextItem != nil {
		item.nextItem.prevItem = item.prevItem
	} else {
		l.lastItem = item.prevItem
	}
	item.prevItem = nil
	item.nextItem = nil
}

func (l *idleConnList) forEach(f func(item *idleConnListItem)) {
	var nextItem *idleConnListItem

	l.mtx.Lock()
	defer l.mtx.Unlock()

	for item := l.firstItem; item != nil; item = nextItem {
		nextItem = item.nextItem
		f(item)
	}
}
