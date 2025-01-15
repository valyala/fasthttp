//go:build darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x)

package tcplisten

import "golang.org/x/sys/unix"

const soReusePort = unix.SO_REUSEPORT

func enableDeferAccept(fd int) error {
	// TODO: implement SO_ACCEPTFILTER:dataready here
	return nil
}

func enableFastOpen(fd int) error {
	// TODO: implement TCP_FASTOPEN when it will be ready
	return nil
}

func soMaxConn() (int, error) {
	// TODO: properly implement it
	return unix.SOMAXCONN, nil
}
