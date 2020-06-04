package reuseport

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

var listenConfig = net.ListenConfig{
	Control: func(network, address string, c syscall.RawConn) (err error) {
		return c.Control(func(fd uintptr) {
			err = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
		})
	},
}

// Listen returns TCP listener with SO_REUSEADDR option set, SO_REUSEPORT is not supported on Windows, so it uses
// SO_REUSEADDR as an alternative to achieve the same effect.
func Listen(network, addr string) (net.Listener, error) {
	return listenConfig.Listen(context.Background(), network, addr)
}
