package fasthttp

import (
	"errors"
	"syscall"
)

func isConnectionReset(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET)
}
