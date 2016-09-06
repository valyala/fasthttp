// +build linux

package reuseport

import (
	"fmt"
	"syscall"
)

const (
	soReusePort = 0x0F
	tcpFastOpen = 0x17
)

func enableDeferAccept(fd int) error {
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_DEFER_ACCEPT, 1); err != nil {
		return fmt.Errorf("cannot enable TCP_DEFER_ACCEPT: %s", err)
	}
	return nil
}

func enableFastOpen(fd int) error {
	if err := syscall.SetsockoptInt(fd, syscall.SOL_TCP, tcpFastOpen, fastOpenQlen); err != nil {
		return fmt.Errorf("cannot enable TCP_FASTOPEN(qlen=%d): %s", fastOpenQlen, err)
	}
	return nil
}

const fastOpenQlen = 16 * 1024
