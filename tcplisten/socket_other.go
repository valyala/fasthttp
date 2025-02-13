//go:build !js && !wasm && (linux || dragonfly || freebsd || netbsd || openbsd || rumprun)

package tcplisten

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func newSocketCloexec(domain, typ, proto int) (int, error) {
	fd, err := unix.Socket(domain, typ|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, proto)
	if err == nil {
		return fd, nil
	}

	if err == unix.EPROTONOSUPPORT || err == unix.EINVAL {
		return newSocketCloexecOld(domain, typ, proto)
	}

	return -1, fmt.Errorf("cannot create listening unblocked socket: %w", err)
}
