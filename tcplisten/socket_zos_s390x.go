//go:build zos && s390x

package tcplisten

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func newSocketCloexec(domain, typ, proto int) (int, error) {
	fd, err := unix.Socket(domain, typ, proto)
	_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, unix.O_NONBLOCK)
	if err == nil {
		return fd, nil
	}
	return -1, fmt.Errorf("cannot create listening unblocked socket: %s", err)
}
