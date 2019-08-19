package reuseport

import (
	"fmt"
	"net"
)

// ErrNoReusePort is returned if the OS doesn't support SO_REUSEPORT.
type ErrNoReusePort struct {
	err error
}

// Error implements error interface.
func (e *ErrNoReusePort) Error() string {
	return fmt.Sprintf("The OS doesn't support SO_REUSEPORT: %s", e.err)
}

// Listen always returns ErrNoReusePort on Windows
func Listen(network, addr string) (net.Listener, error) {
	return nil, &ErrNoReusePort{fmt.Errorf("Not supported on Windows")}
}
