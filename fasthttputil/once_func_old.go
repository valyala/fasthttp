//go:build !go1.21

package fasthttputil

import "sync"

// OnceFunc returns a function that invokes f only once. The returned function may be called concurrently.
func OnceFunc(f func()) func() {
	var once sync.Once
	return func() {
		once.Do(f)
	}
}
