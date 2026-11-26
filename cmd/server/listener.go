package main

import (
	"errors"
	"net"
	"time"
)

type stalledClientListener struct {
	net.Listener
	timeout time.Duration
}

func newStalledClientListener(listener net.Listener, timeout time.Duration) net.Listener {
	if timeout == 0 {
		return listener
	}
	return &stalledClientListener{Listener: listener, timeout: timeout}
}

func (listener *stalledClientListener) Accept() (net.Conn, error) {
	conn, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &stalledClientConn{Conn: conn, timeout: listener.timeout}, nil
}

// Not an io.ReaderFrom on purpose: net/http then copies through Write, so every chunk refreshes the deadline.
type stalledClientConn struct {
	net.Conn
	timeout time.Duration
}

// Resets the deadline on every Write so only stalled clients are dropped; http.Server.WriteTimeout would cap the whole response.
func (conn *stalledClientConn) Write(data []byte) (int, error) {
	if err := conn.SetWriteDeadline(time.Now().Add(conn.timeout)); err != nil {
		return 0, err
	}
	return conn.Conn.Write(data)
}

// CloseWrite keeps net/http's TCP half-close, so an early error response is not lost to a reset.
func (conn *stalledClientConn) CloseWrite() error {
	if closeWriter, ok := conn.Conn.(interface{ CloseWrite() error }); ok {
		return closeWriter.CloseWrite()
	}
	return errors.ErrUnsupported
}
