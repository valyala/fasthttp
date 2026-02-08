//go:build zos && s390x

package listensocket

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// NewSocketCloexec creates a non-blocking socket on z/OS.
func NewSocketCloexec(domain, typ, proto int) (int, error) {
	fd, err := unix.Socket(domain, typ, proto)
	if err != nil {
		return -1, fmt.Errorf("cannot create listening socket: %w", err)
	}

	if _, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("cannot set FD_CLOEXEC: %w", err)
	}

	if _, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, unix.O_NONBLOCK); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("cannot set O_NONBLOCK: %w", err)
	}

	return fd, nil
}
