package smtp

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"neuralmail/internal/emailtransport"
)

func TestSMTPContextCancellationInterruptsGreetingRead(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	adapter := NewOutboundAdapter(Config{Host: host, Port: n})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := adapter.SendMessage(ctx, emailtransport.OutboundMessage{From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "test", TextBody: "synthetic"}, "key")
		done <- err
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("no connection")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled greeting succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP read ignored cancellation")
	}
}
