//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || rumprun

package udplisten

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestConfigDefault(t *testing.T) {
	testConfig(t, Config{})
}

func TestConfigReusePort(t *testing.T) {
	testConfig(t, Config{ReusePort: true})
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
