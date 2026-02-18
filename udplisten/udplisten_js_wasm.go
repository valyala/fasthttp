package udplisten

import "net"

// A dummy implementation for js,wasm
type Config struct {
	ReusePort      bool
	RecvBufferSize int
	SendBufferSize int
}

func (cfg *Config) NewPacketConn(network, addr string) (net.PacketConn, error) {
	return net.ListenPacket(network, addr)
}
