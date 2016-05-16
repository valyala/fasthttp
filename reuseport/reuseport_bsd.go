// +build darwin dragonfly freebsd netbsd openbsd rumprun

package reuseport

import (
	"syscall"
)

const soReusePort = syscall.SO_REUSEPORT
