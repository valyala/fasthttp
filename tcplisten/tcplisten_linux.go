//go:build linux

package tcplisten

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	soReusePort = 0x0F
	tcpFastOpen = 0x17
)

func enableDeferAccept(fd int) error {
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 1); err != nil {
		return fmt.Errorf("cannot enable TCP_DEFER_ACCEPT: %s", err)
	}
	return nil
}

func enableFastOpen(fd int) error {
	if err := unix.SetsockoptInt(fd, unix.SOL_TCP, tcpFastOpen, fastOpenQlen); err != nil {
		return fmt.Errorf("cannot enable TCP_FASTOPEN(qlen=%d): %s", fastOpenQlen, err)
	}
	return nil
}

const fastOpenQlen = 16 * 1024

func soMaxConn() (int, error) {
	data, err := os.ReadFile(soMaxConnFilePath)
	if err != nil {
		// This error may trigger on travis build. Just use SOMAXCONN
		if os.IsNotExist(err) {
			return unix.SOMAXCONN, nil
		}
		return -1, err
	}
	s := strings.TrimSpace(string(data))
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return -1, fmt.Errorf("cannot parse somaxconn %q read from %s: %s", s, soMaxConnFilePath, err)
	}

	// Linux stores the backlog in a uint16.
	// Truncate number to avoid wrapping.
	// See https://github.com/golang/go/issues/5030 .
	if n > 1<<16-1 {
		n = maxAckBacklog(n)
	}
	return n, nil
}

func kernelVersion() (major, minor int) {
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		return
	}

	rl := uname.Release
	var values [2]int
	vi := 0
	value := 0
	for _, c := range rl {
		if c >= '0' && c <= '9' {
			value = (value * 10) + int(c-'0')
		} else {
			// Note that we're assuming N.N.N here.  If we see anything else we are likely to
			// mis-parse it.
			values[vi] = value
			vi++
			if vi >= len(values) {
				break
			}
		}
	}
	switch vi {
	case 0:
		return 0, 0
	case 1:
		return values[0], 0
	case 2:
		return values[0], values[1]
	}
	return
}

// Linux stores the backlog as:
//
//   - uint16 in kernel version < 4.1,
//   - uint32 in kernel version >= 4.1
//
// Truncate number to avoid wrapping.
//
// See issue 5 or
// https://github.com/golang/go/issues/5030.
// https://github.com/golang/go/issues/41470.
func maxAckBacklog(n int) int {
	major, minor := kernelVersion()
	size := 16
	if major > 4 || (major == 4 && minor >= 1) {
		size = 32
	}

	u := 1<<size - 1
	if n > u {
		n = u
	}
	return n
}

const soMaxConnFilePath = "/proc/sys/net/core/somaxconn"
