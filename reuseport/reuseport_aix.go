package reuseport

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

var listenConfig = net.ListenConfig{
	Control: func(network, address string, c syscall.RawConn) (err error) {
		return c.Control(func(fd uintptr) {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if err == nil {
				err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}
		})
	},
}

// Listen returns a TCP listener with the SO_REUSEADDR and SO_REUSEPORT options set.
func Listen(network, addr string) (net.Listener, error) {
	return listenConfig.Listen(context.Background(), network, addr)
}
