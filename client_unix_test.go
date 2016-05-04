// +build darwin dragonfly freebsd linux nacl netbsd openbsd solaris

package fasthttp

import (
	"sync"
	"testing"
)

func TestHostClientGet(t *testing.T) {
	addr := "./TestHostClientGet.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

	testHostClientGet(t, c, 100)
}

func TestHostClientPost(t *testing.T) {
	addr := "./TestHostClientPost.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

	testHostClientPost(t, c, 100)
}

func TestHostClientConcurrent(t *testing.T) {
	addr := "./TestHostClientConcurrent.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testHostClientGet(t, c, 30)
			testHostClientPost(t, c, 10)
		}()
	}
	wg.Wait()
}
