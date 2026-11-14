package fasthttp

import (
	"io"
	"sync"
)

type userDataKV struct {
	key   any
	value any
}

type userData struct {
	// mu guards entries against concurrent reads by RequestCtx.Value.
	// Other readers run on the goroutine that owns the request.
	mu      sync.Mutex
	entries []userDataKV
}

func (d *userData) Set(key, value any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := key.([]byte); ok {
		key = string(b)
	}
	args := d.entries
	n := len(args)
	for i := range n {
		kv := &args[i]
		if kv.key == key {
			kv.value = value
			return
		}
	}

	if value == nil {
		return
	}

	c := cap(args)
	if c > n {
		args = args[:n+1]
		kv := &args[n]
		kv.key = key
		kv.value = value
		d.entries = args
		return
	}

	kv := userDataKV{}
	kv.key = key
	kv.value = value
	args = append(args, kv)
	d.entries = args
}

func (d *userData) SetBytes(key []byte, value any) {
	d.Set(key, value)
}

func (d *userData) Get(key any) any {
	if b, ok := key.([]byte); ok {
		key = b2s(b)
	}
	args := d.entries
	n := len(args)
	for i := range n {
		kv := &args[i]
		if kv.key == key {
			return kv.value
		}
	}
	return nil
}

func (d *userData) GetBytes(key []byte) any {
	return d.Get(key)
}

func (d *userData) Reset() {
	if len(d.entries) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, kv := range d.entries {
		if vc, ok := kv.value.(io.Closer); ok {
			// Close may call back into the request.
			func() {
				d.mu.Unlock()
				defer d.mu.Lock()
				vc.Close()
			}()
		}
		d.entries[i] = userDataKV{}
	}
	d.entries = d.entries[:0]
}

func (d *userData) Remove(key any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := key.([]byte); ok {
		key = b2s(b)
	}
	args := d.entries
	n := len(args)
	for i := 0; i < n; i++ {
		kv := &args[i]
		if kv.key == key {
			n--
			args[i], args[n] = args[n], args[i]
			args[n].key = nil
			args[n].value = nil
			args = args[:n]
			d.entries = args
			return
		}
	}
}

func (d *userData) RemoveBytes(key []byte) {
	d.Remove(key)
}
