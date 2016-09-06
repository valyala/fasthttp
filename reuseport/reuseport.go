// +build linux darwin dragonfly freebsd netbsd openbsd rumprun

// Package reuseport provides TCP net.Listener with SO_REUSEPORT support.
//
// SO_REUSEPORT allows linear scaling server performance on multi-CPU servers.
// See https://www.nginx.com/blog/socket-sharding-nginx-release-1-9-1/ for more details :)
//
// The package is based on https://github.com/kavu/go_reuseport .
package reuseport

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// ErrNoReusePort is returned if the OS doesn't support SO_REUSEPORT.
type ErrNoReusePort struct {
	err error
}

// Error implements error interface.
func (e *ErrNoReusePort) Error() string {
	return fmt.Sprintf("The OS doesn't support SO_REUSEPORT: %s", e.err)
}

// Listen returns TCP listener with SO_REUSEPORT option set.
//
// The returned listener tries enabling the following TCP options, which usually
// have positive impact on performance:
//
// - TCP_DEFER_ACCEPT. This option expects that the server reads from accepted
//   connections before writing to them.
//
// - TCP_FASTOPEN. See https://lwn.net/Articles/508865/ for details.
//
// Only tcp4 and tcp6 networks are supported.
//
// ErrNoReusePort error is returned if the system doesn't support SO_REUSEPORT.
func Listen(network, addr string) (net.Listener, error) {
	sa, soType, err := getSockaddr(network, addr)
	if err != nil {
		return nil, err
	}

	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(soType, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}

	if err = fdSetup(fd, sa, addr); err != nil {
		syscall.Close(fd)
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

func fdSetup(fd int, sa syscall.Sockaddr, addr string) error {
	var err error

	if err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return fmt.Errorf("cannot enable SO_REUSEADDR: %s", err)
	}

	if err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1); err != nil {
		return &ErrNoReusePort{err}
	}

	if err = enableDeferAccept(fd); err != nil {
		return err
	}

	if err = enableFastOpen(fd); err != nil {
		return err
	}

	if err = syscall.Bind(fd, sa); err != nil {
		return fmt.Errorf("cannot bind to %q: %s", addr, err)
	}

	if err = syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		return fmt.Errorf("cannot listen on %q: %s", addr, err)
	}

	return nil
}

func getSockaddr(network, addr string) (sa syscall.Sockaddr, soType int, err error) {
	// TODO: add support for tcp networks.

	if network != "tcp4" && network != "tcp6" {
		return nil, -1, errors.New("only tcp4 and tcp6 network is supported")
	}

	tcpAddr, err := net.ResolveTCPAddr(network, addr)
	if err != nil {
		return nil, -1, err
	}

	switch network {
	case "tcp4":
		var sa4 syscall.SockaddrInet4
		sa4.Port = tcpAddr.Port
		copy(sa4.Addr[:], tcpAddr.IP.To4())
		return &sa4, syscall.AF_INET, nil
	case "tcp6":
		var sa6 syscall.SockaddrInet6
		sa6.Port = tcpAddr.Port
		copy(sa6.Addr[:], tcpAddr.IP.To16())
		if tcpAddr.Zone != "" {
			ifi, err := net.InterfaceByName(tcpAddr.Zone)
			if err != nil {
				return nil, -1, err
			}
			sa6.ZoneId = uint32(ifi.Index)
		}
		return &sa6, syscall.AF_INET6, nil
	default:
		return nil, -1, errors.New("Unknown network type " + network)
	}
}
