package srvconn

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestNewSSHClientContextCancelsHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(io.Discard, conn)
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	client, err := NewSSHClientContext(ctx, SSHClientHost(host), SSHClientPort(portNumber))
	if err == nil || client != nil || ctx.Err() == nil {
		t.Fatal("stalled SSH handshake was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handshake TCP connection leaked")
	}
}
