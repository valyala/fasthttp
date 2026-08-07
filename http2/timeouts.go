package http2

import (
	"errors"
	"net"
	"time"
)

func timerChannel(timer *time.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.C
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type timeoutError struct{ msg string }

func (e timeoutError) Error() string { return e.msg }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var (
	errStreamDeadline = timeoutError{"http2: stream deadline exceeded"}
	errWriteTimeout   = timeoutError{"http2: write timeout"}
)
