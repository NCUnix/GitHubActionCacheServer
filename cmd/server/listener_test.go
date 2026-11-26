package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testDownloadIdleTimeout = 500 * time.Millisecond

func TestStalledClientListenerDropsClientThatStopsReading(t *testing.T) {
	writeErr := make(chan error, 1)
	serveStalledClientRequest(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64*1024)
		for {
			if _, err := writer.Write(chunk); err != nil {
				writeErr <- err
				return
			}
		}
	}))

	select {
	case err := <-writeErr:
		require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("response write did not fail while the client stopped reading")
	}
}

func TestStalledClientConnKeepsSlowReaderConnected(t *testing.T) {
	const (
		timeout      = time.Second
		readInterval = 10 * time.Millisecond
		chunkSize    = 32 * 1024
		// Sleeps between reads keep the writes running for at least 2 timeouts while each Write waits about one interval.
		writes = 2*int(timeout/readInterval) + 1
	)
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	conn := &stalledClientConn{Conn: server, timeout: timeout}

	type readResult struct {
		received int
		err      error
	}
	result := make(chan readResult, 1)
	go func() {
		// A chunk-sized buffer lets a single Read consume each Write.
		buffer := make([]byte, chunkSize)
		received := 0
		for {
			count, err := client.Read(buffer)
			received += count
			if err != nil {
				result <- readResult{received: received, err: err}
				return
			}
			time.Sleep(readInterval)
		}
	}()

	chunk := make([]byte, chunkSize)
	started := time.Now()
	for range writes {
		_, err := conn.Write(chunk)
		require.NoError(t, err)
	}
	elapsed := time.Since(started)
	require.NoError(t, conn.Close())

	read := <-result
	require.ErrorIs(t, read.err, io.EOF)
	require.Equal(t, writes*chunkSize, read.received)
	require.Greater(t, elapsed, timeout)
}

func TestStalledClientConnDropsReaderThatStopsReading(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	conn := &stalledClientConn{Conn: server, timeout: 50 * time.Millisecond}

	writeErr := make(chan error, 1)
	go func() {
		_, err := conn.Write(make([]byte, 1))
		writeErr <- err
	}()

	select {
	case err := <-writeErr:
		require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("write did not fail while the reader stopped reading")
	}
}

func TestNewStalledClientListenerWithoutTimeoutReturnsListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	require.Same(t, listener, newStalledClientListener(listener, 0))
}

func TestStalledClientConnForwardsCloseWriteWithoutReaderFrom(t *testing.T) {
	listener := listenStalledClient(t)
	client, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	conn, err := listener.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, isReaderFrom := conn.(io.ReaderFrom)
	require.False(t, isReaderFrom)
	closeWriter, ok := conn.(interface{ CloseWrite() error })
	require.True(t, ok)
	require.NoError(t, closeWriter.CloseWrite())
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = client.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

func listenStalledClient(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listener = newStalledClientListener(listener, testDownloadIdleTimeout)
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func serveStalledClientRequest(t *testing.T, handler http.Handler) {
	t.Helper()
	listener := listenStalledClient(t)
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	client, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = io.WriteString(client, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
}
