// +build linux

package reuseport

import (
	"syscall"
)

var reusePort = 0x0F

func setTCPDeferAccept(fd int) error {
	return syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.TCP_DEFER_ACCEPT, 2)
}
