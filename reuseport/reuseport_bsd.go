// +build darwin dragonfly freebsd netbsd openbsd

package reuseport

var reusePort = syscall.SO_REUSEPORT

func setTCPDeferAccept(fd int) error {
	// BSD supports similar SO_ACCEPTFILTER option, but I have no access
	// to BSD at the moment for proper implementation.
	return nil
}
