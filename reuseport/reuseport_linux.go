// +build linux

package reuseport

import "syscall"

const soReusePort = 0x0F

func tcpDeferAccept(fd int) error {
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_DEFER_ACCEPT, 1)
}
