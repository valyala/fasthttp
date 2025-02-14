package tcplisten

import (
	"net"
)

// A dummy implementation for js,wasm
type Config struct {
	ReusePort   bool
	DeferAccept bool
	FastOpen    bool
	Backlog     int
}

func (cfg *Config) NewListener(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}
