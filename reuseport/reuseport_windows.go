package reuseport

import (
	"fmt"
	"net"
)

// Listen always returns ErrNoReusePort on Windows
func Listen(network, addr string) (net.Listener, error) {
	return nil, &ErrNoReusePort{fmt.Errorf("Not supported on Windows")}
}
