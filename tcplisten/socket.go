//go:build !js && !wasm && (linux || darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x))

package tcplisten

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func newSocketCloexecOld(domain, typ, proto int) (int, error) {
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(domain, typ, proto)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, fmt.Errorf("cannot create listening socket: %w", err)
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("cannot make non-blocked listening socket: %w", err)
	}
	return fd, nil
}
