//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || rumprun || (zos && s390x)

package udplisten

import (
	"fmt"
	"net"
)

func ExampleConfig_NewPacketConn() {
	cfg := Config{
		ReusePort: true,
	}
	pc, err := cfg.NewPacketConn("udp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer pc.Close()

	// Use a channel to wait for the echo to complete
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		_, _ = pc.WriteTo(buf[:n], addr)
	}()
	conn, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	if _, err = fmt.Fprint(conn, "echo"); err != nil {
		panic(err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		panic(err)
	}
	fmt.Printf("response: %s\n", buf[:n])

	<-done // Wait for the echo goroutine to complete
	// Output: response: echo
}
