// +build darwin dragonfly freebsd netbsd openbsd

package reuseport

import (
	"syscall"
)

const SO_REUSEPORT = syscall.SO_REUSEPORT
