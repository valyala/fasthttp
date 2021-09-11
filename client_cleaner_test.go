package fasthttp

import (
	"testing"
)

type cleanItem struct {
	used int
	free int
}

func (ci *cleanItem) cleanResource() {
	ci.free = 0
}

func (ci *cleanItem) hasResource() bool {
	total := ci.used + ci.free
	return total > 0
}

func TestClientCleaner(t *testing.T) {
	c1 := &cleanItem{used: 1, free: 0}
	c2 := &cleanItem{used: 2, free: 0}

	exists := func(ci *cleanItem) bool {
		var item resourceClean = ci
		_, ok := cCleaner.clients.Load(item)
		return ok
	}

	// test register
	cCleaner.register(c1)
	cCleaner.register(c2)
	if !exists(c1) {
		t.Errorf("clientCleaner error, item register but not exist.")
	}
	if !exists(c2) {
		t.Errorf("clientCleaner error, item register but not exist.")
	}

	// test clean
	c1.used = 0
	cCleaner.cleanClient()
	if exists(c1) {
		t.Errorf("clientCleaner error, item has no resource but not deleted")
	}

	// test duplicate register
	c1.used, c1.free = 0, 3
	cCleaner.register(c1)
	cCleaner.register(c1)
	if !exists(c1) {
		t.Errorf("clientCleaner error, item register but not exist.")
	}

	// test clean and delete
	cCleaner.cleanClient()
	if exists(c1) {
		t.Errorf("clientCleaner error, item has no resource but not deleted")
	}

	if !exists(c2) {
		t.Errorf("clientCleaner error, item register but not exist.")
	}
}