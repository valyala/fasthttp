package fasthttp

import (
	"net"
	"time"
)

type TimeoutListener struct {
	// The original listener.
	Listener net.Listener

	// Maximum wait time for each read() operation on accepted connections.
	//
	// By default read timeout is disabled.
	ReadTimeout time.Duration

	// Maximum wait time for each write() operation on accepted connections.
	//
	// By default write timeout is disabled.
	WriteTimeout time.Duration
}

func (ln *TimeoutListener) Accept() (net.Conn, error) {
	c, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &timeoutConn{
		Conn:         c,
		readTimeout:  ln.ReadTimeout,
		writeTimeout: ln.WriteTimeout,
	}, nil
}

func (ln *TimeoutListener) Addr() net.Addr {
	return ln.Listener.Addr()
}

func (ln *TimeoutListener) Close() error {
	return ln.Listener.Close()
}

type timeoutConn struct {
	net.Conn
	readTimeout  time.Duration
	writeTimeout time.Duration
}

func (c *timeoutConn) Write(b []byte) (int, error) {
	if c.writeTimeout > 0 {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(b)
}

func (c *timeoutConn) Read(b []byte) (int, error) {
	if c.readTimeout > 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(b)
}
