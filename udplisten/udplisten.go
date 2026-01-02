//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x)

// Package udplisten provides customizable UDP net.PacketConn with various
// performance-related options:
//
//   - SO_REUSEPORT. This option allows linear scaling server performance
//     on multi-CPU servers.
//
//   - SO_RCVBUF and SO_SNDBUF. These options allow tuning socket buffer sizes.
package udplisten

import (
	"fmt"
	"net"
	"os"

	"github.com/valyala/fasthttp/internal/listensocket"
	"golang.org/x/sys/unix"
)

// Config provides options to enable on the returned net.PacketConn.
type Config struct {
	// ReusePort enables SO_REUSEPORT.
	ReusePort bool

	// RecvBufferSize sets SO_RCVBUF when greater than zero.
	RecvBufferSize int

	// SendBufferSize sets SO_SNDBUF when greater than zero.
	SendBufferSize int
}

// NewPacketConn returns UDP PacketConn with options set in the Config.
//
// The function may be called many times for creating distinct PacketConns
// with the given config.
//
// Only udp4 and udp6 networks are supported.
func (cfg *Config) NewPacketConn(network, addr string) (net.PacketConn, error) {
	sa, soType, err := getSockaddr(network, addr)
	if err != nil {
		return nil, err
	}

	fd, err := listensocket.NewSocketCloexec(soType, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return nil, err
	}

	if err = cfg.fdSetup(fd, sa, addr); err != nil {
		unix.Close(fd)
		return nil, err
	}

	name := fmt.Sprintf("reuseport.%d.%s.%s", os.Getpid(), network, addr)
	file := os.NewFile(uintptr(fd), name)
	pc, err := net.FilePacketConn(file)
	if err != nil {
		file.Close()
		return nil, err
	}

	if err = file.Close(); err != nil {
		pc.Close()
		return nil, err
	}

	return pc, nil
}

func (cfg *Config) fdSetup(fd int, sa unix.Sockaddr, addr string) error {
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return fmt.Errorf("cannot enable SO_REUSEADDR: %w", err)
	}

	if cfg.ReusePort {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, soReusePort, 1); err != nil {
			return fmt.Errorf("cannot enable SO_REUSEPORT: %w", err)
		}
	}

	if cfg.RecvBufferSize > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, cfg.RecvBufferSize); err != nil {
			return fmt.Errorf("cannot set SO_RCVBUF: %w", err)
		}
	}

	if cfg.SendBufferSize > 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, cfg.SendBufferSize); err != nil {
			return fmt.Errorf("cannot set SO_SNDBUF: %w", err)
		}
	}

	if err := unix.Bind(fd, sa); err != nil {
		return fmt.Errorf("cannot bind to %q: %w", addr, err)
	}

	return nil
}

func getSockaddr(network, addr string) (sa unix.Sockaddr, soType int, err error) {
	udpAddr, err := net.ResolveUDPAddr(network, addr)
	if err != nil {
		return nil, -1, err
	}

	switch network {
	case "udp4":
		var sa4 unix.SockaddrInet4
		sa4.Port = udpAddr.Port
		copy(sa4.Addr[:], udpAddr.IP.To4())
		return &sa4, unix.AF_INET, nil
	case "udp6":
		var sa6 unix.SockaddrInet6
		sa6.Port = udpAddr.Port
		copy(sa6.Addr[:], udpAddr.IP.To16())
		if udpAddr.Zone != "" {
			ifi, err := net.InterfaceByName(udpAddr.Zone)
			if err != nil {
				return nil, -1, err
			}
			sa6.ZoneId, err = listensocket.SafeIntToUint32(ifi.Index)
			if err != nil {
				return nil, -1, fmt.Errorf("unexpected convert net interface index int to uint32: %w", err)
			}
		}
		return &sa6, unix.AF_INET6, nil
	case "udp":
		var sa6 unix.SockaddrInet6
		sa6.Port = udpAddr.Port
		if udpAddr.IP == nil {
			udpAddr.IP = net.IPv4(0, 0, 0, 0)
		}
		copy(sa6.Addr[:], udpAddr.IP.To16())
		if udpAddr.Zone != "" {
			ifi, err := net.InterfaceByName(udpAddr.Zone)
			if err != nil {
				return nil, -1, err
			}
			sa6.ZoneId, err = listensocket.SafeIntToUint32(ifi.Index)
			if err != nil {
				return nil, -1, fmt.Errorf("unexpected convert net interface index int to uint32: %w", err)
			}
		}
		return &sa6, unix.AF_INET6, nil
	default:
		return nil, -1, fmt.Errorf("only udp, udp4, or udp6 is supported %s", network)
	}
}
