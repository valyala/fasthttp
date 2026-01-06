//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x)

package udplisten

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConfigDefault(t *testing.T) {
	testConfig(t, Config{})
}

func TestConfigReusePort(t *testing.T) {
	testConfig(t, Config{ReusePort: true})
}

func TestConfigBufferSizes(t *testing.T) {
	testBufferSizes(t, Config{
		RecvBufferSize: 8192,
		SendBufferSize: 8192,
	})
}

func TestConfigBufferSizesWithReusePort(t *testing.T) {
	testBufferSizes(t, Config{
		ReusePort:      true,
		RecvBufferSize: 16384,
		SendBufferSize: 16384,
	})
}

// verifySocketBufferSize checks if the given socket option value meets the minimum required size.
func verifySocketBufferSize(fd uintptr, optname int, optStr string, minSize int) (int, error) {
	actualSize, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, optname)
	if err != nil {
		return 0, err
	}
	// The kernel may double the value we set, so check it's at least what we requested
	if actualSize < minSize {
		return 0, fmt.Errorf("%s is %d, expected at least %d", optStr, actualSize, minSize)
	}
	return actualSize, nil
}

func testBufferSizes(t *testing.T, cfg Config) {
	networks := []struct {
		network string
		addr    string
	}{
		{"udp4", "127.0.0.1:0"},
		{"udp6", "[::1]:0"},
	}

	for _, nw := range networks {
		t.Run(nw.network, func(t *testing.T) {
			pc, err := cfg.NewPacketConn(nw.network, nw.addr)
			if err != nil {
				t.Fatalf("cannot create packet conn using Config %#v: %s", &cfg, err)
			}
			defer pc.Close()

			// Verify buffer sizes are set correctly
			if cfg.RecvBufferSize > 0 || cfg.SendBufferSize > 0 {
				conn, ok := pc.(*net.UDPConn)
				if !ok {
					t.Fatalf("expected *net.UDPConn, got %T", pc)
				}

				rawConn, err := conn.SyscallConn()
				if err != nil {
					t.Fatalf("failed to get raw conn: %s", err)
				}

				var recvBufSize, sendBufSize int
				var ctrlErr error
				err = rawConn.Control(func(fd uintptr) {
					if cfg.RecvBufferSize > 0 {
						recvBufSize, ctrlErr = verifySocketBufferSize(fd, unix.SO_RCVBUF, "SO_RCVBUF", cfg.RecvBufferSize)
						if ctrlErr != nil {
							return
						}
					}

					if cfg.SendBufferSize > 0 {
						sendBufSize, ctrlErr = verifySocketBufferSize(fd, unix.SO_SNDBUF, "SO_SNDBUF", cfg.SendBufferSize)
					}
				})
				if err != nil {
					t.Fatalf("failed to control raw conn: %s", err)
				}
				if ctrlErr != nil {
					t.Fatalf("failed to verify buffer sizes: %s", ctrlErr)
				}

				if cfg.RecvBufferSize > 0 && cfg.SendBufferSize > 0 {
					t.Logf("Verified buffer sizes: SO_RCVBUF=%d (requested %d), SO_SNDBUF=%d (requested %d)",
						recvBufSize, cfg.RecvBufferSize, sendBufSize, cfg.SendBufferSize)
				} else if cfg.RecvBufferSize > 0 {
					t.Logf("Verified buffer size: SO_RCVBUF=%d (requested %d)", recvBufSize, cfg.RecvBufferSize)
				} else {
					t.Logf("Verified buffer size: SO_SNDBUF=%d (requested %d)", sendBufSize, cfg.SendBufferSize)
				}
			}

			// Test that the connection works with basic echo
			bindAddr := pc.LocalAddr().String()
			c, err := net.Dial(nw.network, bindAddr)
			if err != nil {
				t.Fatalf("unexpected error when dialing: %s", err)
			}
			defer c.Close()

			// Send a test message
			testMsg := "buffer size test"
			if err = c.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatalf("unexpected error when setting deadline: %s", err)
			}

			// Start echo server
			errCh := make(chan error, 1)
			go func() {
				buf := make([]byte, 1024)
				_ = pc.SetReadDeadline(time.Now().Add(time.Second))
				n, addr, err := pc.ReadFrom(buf)
				if err != nil {
					errCh <- err
					return
				}
				if _, err = pc.WriteTo(buf[:n], addr); err != nil {
					errCh <- err
					return
				}
				errCh <- nil
			}()

			if _, err = c.Write([]byte(testMsg)); err != nil {
				t.Fatalf("unexpected error when writing request: %s", err)
			}

			resp := make([]byte, len(testMsg))
			n, err := c.Read(resp)
			if err != nil {
				t.Fatalf("unexpected error when reading response: %s", err)
			}
			if string(resp[:n]) != testMsg {
				t.Fatalf("unexpected response %q. Expecting %q", resp[:n], testMsg)
			}

			if err := <-errCh; err != nil {
				t.Fatalf("echo server error: %s", err)
			}
		})
	}
}

func testConfig(t *testing.T, cfg Config) {
	testConfigV(t, cfg, "udp", "127.0.0.1:0")
	testConfigV(t, cfg, "udp4", "127.0.0.1:0")
	testConfigV(t, cfg, "udp6", "[::1]:0")
}

func testConfigV(t *testing.T, cfg Config, network, addr string) {
	const requestsCount = 200
	serversCount := 1
	if cfg.ReusePort {
		serversCount = 5
	}
	doneCh := make(chan struct{}, serversCount)

	pc, err := cfg.NewPacketConn(network, addr)
	if err != nil {
		t.Fatalf("cannot create packet conn using Config %#v: %s", &cfg, err)
	}

	udpAddr, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("unexpected local addr type %T", pc.LocalAddr())
	}
	bindAddr := udpAddr.String()

	go func() {
		serveUDPEcho(t, pc)
		doneCh <- struct{}{}
	}()
	pcs := []net.PacketConn{pc}

	for i := 1; i < serversCount; i++ {
		conn, err := cfg.NewPacketConn(network, bindAddr)
		if err != nil {
			t.Fatalf("cannot create packet conn %d using Config %#v: %s", i, &cfg, err)
		}
		pcs = append(pcs, conn)
		go func(pc net.PacketConn) {
			serveUDPEcho(t, pc)
			doneCh <- struct{}{}
		}(conn)
	}

	for i := 0; i < requestsCount; i++ {
		c, err := net.Dial(network, bindAddr)
		if err != nil {
			t.Fatalf("%d. unexpected error when dialing: %s", i, err)
		}
		req := fmt.Sprintf("request number %d", i)
		if err = c.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("%d. unexpected error when setting deadline: %s", i, err)
		}
		if _, err = c.Write([]byte(req)); err != nil {
			t.Fatalf("%d. unexpected error when writing request: %s", i, err)
		}

		resp := make([]byte, len(req))
		n, err := c.Read(resp)
		if err != nil {
			t.Fatalf("%d. unexpected error when reading response: %s", i, err)
		}
		if string(resp[:n]) != req {
			t.Fatalf("%d. unexpected response %q. Expecting %q", i, resp[:n], req)
		}
		if err = c.Close(); err != nil {
			t.Fatalf("%d. unexpected error when closing connection: %s", i, err)
		}
	}

	for _, pc := range pcs {
		if err := pc.Close(); err != nil {
			t.Fatalf("unexpected error when closing conn: %s", err)
		}
	}

	for i := 0; i < serversCount; i++ {
		select {
		case <-doneCh:
		case <-time.After(time.Second):
			t.Fatalf("timeout when waiting for servers to be closed")
		}
	}
}

func serveUDPEcho(t *testing.T, pc net.PacketConn) {
	buf := make([]byte, 1024)
	for {
		_ = pc.SetReadDeadline(time.Now().Add(time.Second))
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			t.Fatalf("unexpected error when reading request: %s", err)
		}
		if _, err = pc.WriteTo(buf[:n], addr); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			t.Fatalf("unexpected error when writing response: %s", err)
		}
	}
}
