// +build darwin dragonfly freebsd netbsd openbsd

package reuseport

import "syscall"

var reusePort = syscall.SO_REUSEPORT
