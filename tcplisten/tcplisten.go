//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x)

// Package tcplisten provides customizable TCP net.Listener with various
// performance-related options:
//
//   - SO_REUSEPORT. This option allows linear scaling server performance
//     on multi-CPU servers.
//     See https://www.nginx.com/blog/socket-sharding-nginx-release-1-9-1/ for details.
//
//   - TCP_DEFER_ACCEPT. This option expects the server reads from the accepted
//     connection before writing to them.
//
//   - TCP_FASTOPEN. See https://lwn.net/Articles/508865/ for details.
//
// The package is derived from https://github.com/valyala/tcplisten
package tcplisten

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Config provides options to enable on the returned listener.
type Config struct {
	// ReusePort enables SO_REUSEPORT.
	ReusePort bool

	// DeferAccept enables TCP_DEFER_ACCEPT.
	DeferAccept bool

	// FastOpen enables TCP_FASTOPEN.
	FastOpen bool

	// Backlog is the maximum number of pending TCP connections the listener
	// may queue before passing them to Accept.
	// See man 2 listen for details.
	//
	// By default system-level backlog value is used.
	Backlog int
}

// NewListener returns TCP listener with options set in the Config.
//
// The function may be called many times for creating distinct listeners
// with the given config.
//
// Only tcp4 and tcp6 networks are supported.
func (cfg *Config) NewListener(network, addr string) (net.Listener, error) {
	sa, soType, err := getSockaddr(network, addr)
	if err != nil {
		return nil, err
	}

	fd, err := newSocketCloexec(soType, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	if err != nil {
		return nil, err
	}

	if err = cfg.fdSetup(fd, sa, addr); err != nil {
		unix.Close(fd)
		return nil, err
	}

	name := fmt.Sprintf("reuseport.%d.%s.%s", os.Getpid(), network, addr)
	file := os.NewFile(uintptr(fd), name)
	ln, err := net.FileListener(file)
	if err != nil {
		file.Close()
		return nil, err
	}

	if err = file.Close(); err != nil {
		ln.Close()
		return nil, err
	}

	return ln, nil
}

func (cfg *Config) fdSetup(fd int, sa unix.Sockaddr, addr string) error {
	var err error

	if err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return fmt.Errorf("cannot enable SO_REUSEADDR: %w", err)
	}

	// This should disable Nagle's algorithm in all accepted sockets by default.
	// Users may enable it with net.TCPConn.SetNoDelay(false).
	if err = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
		return fmt.Errorf("cannot disable Nagle's algorithm: %w", err)
	}

	if cfg.ReusePort {
		if err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, soReusePort, 1); err != nil {
			return fmt.Errorf("cannot enable SO_REUSEPORT: %w", err)
		}
	}

	if cfg.DeferAccept {
		if err = enableDeferAccept(fd); err != nil {
			return err
		}
	}

	if cfg.FastOpen {
		if err = enableFastOpen(fd); err != nil {
			return err
		}
	}

	if err = unix.Bind(fd, sa); err != nil {
		return fmt.Errorf("cannot bind to %q: %w", addr, err)
	}

	backlog := cfg.Backlog
	if backlog <= 0 {
		if backlog, err = soMaxConn(); err != nil {
			return fmt.Errorf("cannot determine backlog to pass to listen(2): %w", err)
		}
	}
	if err = unix.Listen(fd, backlog); err != nil {
		return fmt.Errorf("cannot listen on %q: %w", addr, err)
	}

	return nil
}

func getSockaddr(network, addr string) (sa unix.Sockaddr, soType int, err error) {
	tcpAddr, err := net.ResolveTCPAddr(network, addr)
	if err != nil {
		return nil, -1, err
	}

	switch network {
	case "tcp4":
		var sa4 unix.SockaddrInet4
		sa4.Port = tcpAddr.Port
		copy(sa4.Addr[:], tcpAddr.IP.To4())
		return &sa4, unix.AF_INET, nil
	case "tcp6":
		var sa6 unix.SockaddrInet6
		sa6.Port = tcpAddr.Port
		copy(sa6.Addr[:], tcpAddr.IP.To16())
		if tcpAddr.Zone != "" {
			ifi, err := net.InterfaceByName(tcpAddr.Zone)
			if err != nil {
				return nil, -1, err
			}
			sa6.ZoneId, err = safeIntToUint32(ifi.Index)
			if err != nil {
				return nil, -1, fmt.Errorf("unexpected convert net interface index int to uint32: %w", err)
			}
		}
		return &sa6, unix.AF_INET6, nil
	case "tcp":
		var sa6 unix.SockaddrInet6
		sa6.Port = tcpAddr.Port
		if tcpAddr.IP == nil {
			tcpAddr.IP = net.IPv4(0, 0, 0, 0)
		}
		copy(sa6.Addr[:], tcpAddr.IP.To16())
		if tcpAddr.Zone != "" {
			ifi, err := net.InterfaceByName(tcpAddr.Zone)
			if err != nil {
				return nil, -1, err
			}
			sa6.ZoneId, err = safeIntToUint32(ifi.Index)
			if err != nil {
				return nil, -1, fmt.Errorf("unexpected convert net interface index int to uint32: %w", err)
			}
		}
		return &sa6, unix.AF_INET6, nil
	default:
		return nil, -1, errors.New("only tcp, tcp4, or tcp6 is supported " + network)
	}
}

func safeIntToUint32(i int) (uint32, error) {
	if i < 0 {
		return 0, errors.New("value is negative, cannot convert to uint32")
	}
	ui := uint64(i)
	if ui > math.MaxUint32 {
		return 0, errors.New("value exceeds uint32 max value")
	}
	return uint32(ui), nil
}
