//go:build darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x)

package udplisten

import "golang.org/x/sys/unix"

const soReusePort = unix.SO_REUSEPORT
