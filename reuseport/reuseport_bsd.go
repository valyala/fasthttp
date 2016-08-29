// +build darwin dragonfly freebsd netbsd openbsd rumprun

package reuseport

import (
	"syscall"
)

const soReusePort = syscall.SO_REUSEPORT

func tcpDeferAccept(fd int) error {
	// TODO: implement SO_ACCEPTFILTER:dataready here
	return nil
}
