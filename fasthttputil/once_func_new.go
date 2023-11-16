//go:build go1.21

package fasthttputil

import "sync"

// OnceFunc returns a function that invokes f only once. The returned function may be called concurrently.
//
// If f panics, the returned function will panic with the same value on every call.
func OnceFunc(f func()) func() {
	return sync.OnceFunc(f)
}
