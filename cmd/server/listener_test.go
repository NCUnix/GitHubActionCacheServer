package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testDownloadIdleTimeout = 500 * time.Millisecond
	testSocketBufferSize    = 16 * 1024
)

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

func TestStalledClientListenerKeepsSlowReaderConnected(t *testing.T) {
	const bodySize = 4 * 1024 * 1024
	type writeResult struct {
		elapsed time.Duration
		err     error
	}
	result := make(chan writeResult, 1)
	client := serveStalledClientRequest(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", strconv.Itoa(bodySize))
		chunk := make([]byte, 32*1024)
		started := time.Now()
		var err error
		for written := 0; written < bodySize && err == nil; written += len(chunk) {
			_, err = writer.Write(chunk)
		}
		result <- writeResult{elapsed: time.Since(started), err: err}
	}))

	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	require.NoError(t, err)
	defer response.Body.Close()
	// Draining the whole receive buffer per read avoids loopback window updates stalling on partial reads.
	buffer := make([]byte, 4*testSocketBufferSize)
	received := 0
	for {
		count, err := response.Body.Read(buffer)
		received += count
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, bodySize, received)

	select {
	case written := <-result:
		require.NoError(t, written.err)
		require.Greater(t, written.elapsed, testDownloadIdleTimeout)
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish writing")
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

func serveStalledClientRequest(t *testing.T, handler http.Handler) net.Conn {
	t.Helper()
	listener := listenStalledClient(t)
	server := &http.Server{
		Handler: handler,
		// Shrink socket buffers so the body outgrows the bytes the kernel keeps in flight.
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateNew {
				_ = conn.(*stalledClientConn).Conn.(*net.TCPConn).SetWriteBuffer(testSocketBufferSize)
			}
		},
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	client, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.(*net.TCPConn).SetReadBuffer(testSocketBufferSize))
	require.NoError(t, client.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = io.WriteString(client, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	return client
}
