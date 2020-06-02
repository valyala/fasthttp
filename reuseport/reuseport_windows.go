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

// Listen always returns ErrNoReusePort on Windows
func Listen(network, addr string) (net.Listener, error) {
	return listenConfig.Listen(context.Background(), network, addr)
}
