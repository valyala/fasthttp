// +build darwin dragonfly freebsd netbsd openbsd

package reuseport

var reusePort = syscall.SO_REUSEPORT
