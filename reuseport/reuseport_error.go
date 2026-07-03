package reuseport

import (
	"fmt"
)

// ErrNoReusePort is returned if the OS doesn't support SO_REUSEPORT.
type ErrNoReusePort struct {
	err error
}

// Error implements error interface.
func (e *ErrNoReusePort) Error() string {
	return fmt.Sprintf("the os doesn't support so_reuseport: %v", e.err)
}
