package http2

import (
	"errors"
	"net"
	"os"
	"time"
)

type timeoutError struct {
	msg string
	// wrapped lets a StreamConn deadline error match os.ErrDeadlineExceeded,
	// as net.Conn requires.
	wrapped error
}

func (e timeoutError) Error() string { return e.msg }
func (e timeoutError) Unwrap() error { return e.wrapped }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var (
	errStreamTimeout = timeoutError{msg: "http2: stream deadline exceeded", wrapped: os.ErrDeadlineExceeded}
	errWriteTimeout  = timeoutError{msg: "http2: write timeout"}
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
