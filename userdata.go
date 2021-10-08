package fasthttp

import (
	"io"
	"sync/atomic"
)

const (
	statusAlive = iota
	statusLocked
	statusDeleted
)

type userDataKV struct {
	key    []byte
	value  interface{}
	status uint32
}

type userData []userDataKV

func (d *userData) Set(key string, value interface{}) {
	args := *d
	n := len(args)
	idx := -1 // the index of a deleted userDataKV in userData. for memory reuse.

	defer func() {
		if idx == -1 {
			return
		}
		// unlock the locked userDataKV
		atomic.CompareAndSwapUint32(&args[idx].status, statusLocked, statusDeleted)
	}()

	for i := 0; i < n; i++ {
		kv := &args[i]
		if string(kv.key) == key {
			kv.value = value
			kv.status = statusAlive
			return
		}
		// lock the locked userDataKV and record its index.
		if idx == -1 {
			if ok := atomic.CompareAndSwapUint32(&kv.status, statusDeleted, statusLocked); ok {
				idx = i
			}
		}
	}

	// reuse
	if idx != -1 {
		kv := &args[idx]
		if kv.status == statusLocked {
			kv.key = append(kv.key[:0], key...)
			kv.value = value
			kv.status = statusAlive
		}
	}

	if value == nil {
		return
	}

	c := cap(args)
	if c > n {
		args = args[:n+1]
		kv := &args[n]
		kv.key = append(kv.key[:0], key...)
		kv.value = value
		*d = args
		return
	}

	kv := userDataKV{}
	kv.key = append(kv.key[:0], key...)
	kv.value = value
	*d = append(args, kv)
}

func (d *userData) SetBytes(key []byte, value interface{}) {
	d.Set(b2s(key), value)
}

func (d *userData) Get(key string) interface{} {
	args := *d
	n := len(args)
	for i := 0; i < n; i++ {
		kv := &args[i]
		if string(kv.key) == key && kv.status == statusAlive {
			return kv.value
		}
	}
	return nil
}

func (d *userData) GetBytes(key []byte) interface{} {
	return d.Get(b2s(key))
}

func (d *userData) Reset() {
	args := *d
	n := len(args)
	for i := 0; i < n; i++ {
		v := args[i].value
		if vc, ok := v.(io.Closer); ok {
			vc.Close()
		}
	}
	*d = (*d)[:0]
}

func (d *userData) Remove(key string) {
	args := *d
	n := len(args)
	for i := 0; i < n; i++ {
		kv := &args[i]
		if string(kv.key) == key {
			kv.status = statusDeleted
			if vc, ok := kv.value.(io.Closer); ok {
				vc.Close()
			}
			return
		}
	}
}

func (d *userData) RemoveBytes(key []byte) {
	d.Remove(b2s(key))
}
