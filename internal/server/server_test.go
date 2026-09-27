package server_test

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"testing"
	"time"

	"quic_server_go/internal/alpn"
	"quic_server_go/internal/server"
	"quic_server_go/internal/testutil"
	"quic_server_go/internal/tlsconfig"
)

func TestListenRequiresTLS(t *testing.T) {
	t.Parallel()

	_, err := server.Listen(server.Config{Addr: "127.0.0.1:0"})
	if err == nil {
		t.Fatal("expected error for nil TLS config")
	}
}

func TestListenRequiresALPN(t *testing.T) {
	t.Parallel()

	_, err := server.Listen(server.Config{
		Addr:      "127.0.0.1:0",
		TLSConfig: &tls.Config{},
	})
	if err == nil {
		t.Fatal("expected error for empty NextProtos")
	}
}

func TestServeReceivesMessage(t *testing.T) {
	t.Parallel()

	tlsSrv, err := tlsconfig.NewServer(alpn.Protocol)
	if err != nil {
		t.Fatalf("tls: %v", err)
	}

	msgCh := make(chan received, 1)
	srv, err := server.Listen(server.Config{
		Addr:      "127.0.0.1:0",
		TLSConfig: tlsSrv,
		OnMessage: func(remote net.Addr, data []byte) {
			msgCh <- received{addr: remote.String(), data: string(data)}
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()

	tr := testutil.NewTransport(t)
	qconn, err := tr.Dial(ctx, srv.Addr(), tlsconfig.NewInsecureClient(alpn.Protocol), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer qconn.CloseWithError(0, "done")

	stream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	const payload = "ping"
	if _, err := stream.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case got := <-msgCh:
		if got.data != payload {
			t.Fatalf("payload: got %q, want %q", got.data, payload)
		}
		if got.addr == "" {
			t.Fatal("empty remote addr")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for message")
	}

	_ = srv.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}

type received struct {
	addr string
	data string
}
