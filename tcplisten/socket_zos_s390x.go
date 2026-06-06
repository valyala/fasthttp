//go:build zos && s390x

package tcplisten

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func newSocketCloexec(domain, typ, proto int) (int, error) {
	fd, err := unix.Socket(domain, typ, proto)
	if err != nil {
		return -1, fmt.Errorf("cannot create listening socket: %w", err)
	}
	_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	if err != nil {
		unix.Close(fd) //nolint:errcheck
		return -1, fmt.Errorf("cannot mark listening socket close-on-exec: %w", err)
	}
	_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, unix.O_NONBLOCK)
	if err != nil {
		unix.Close(fd) //nolint:errcheck
		return -1, fmt.Errorf("cannot mark listening socket nonblocking: %w", err)
	}
	return fd, nil
}
