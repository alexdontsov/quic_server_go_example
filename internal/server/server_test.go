package server_test

import (
	"context"
	"crypto/tls"
	"log/slog"
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

	msgCh := make(chan server.Message, 1)
	srv, err := server.Listen(server.Config{
		Addr:      "127.0.0.1:0",
		TLSConfig: tlsSrv,
		OnMessage: func(m server.Message) { msgCh <- m },
		Logger:    slog.New(slog.DiscardHandler),
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
		if string(got.Data) != payload {
			t.Fatalf("payload: got %q, want %q", got.Data, payload)
		}
		if got.Remote == nil || got.Remote.String() == "" {
			t.Fatal("empty remote addr")
		}
		if got.StreamID != stream.StreamID() {
			t.Fatalf("stream id: got %d, want %d", got.StreamID, stream.StreamID())
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

// Стримы одного соединения обрабатываются независимо: сервер видит данные
// из обоих, не дожидаясь закрытия первого.
func TestServeHandlesConcurrentStreams(t *testing.T) {
	t.Parallel()

	tlsSrv, err := tlsconfig.NewServer(alpn.Protocol)
	if err != nil {
		t.Fatalf("tls: %v", err)
	}

	msgCh := make(chan server.Message, 16)
	srv, err := server.Listen(server.Config{
		Addr:      "127.0.0.1:0",
		TLSConfig: tlsSrv,
		OnMessage: func(m server.Message) { msgCh <- m },
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

	tr := testutil.NewTransport(t)
	qconn, err := tr.Dial(ctx, srv.Addr(), tlsconfig.NewInsecureClient(alpn.Protocol), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer qconn.CloseWithError(0, "done")

	first, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	second, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open second stream: %v", err)
	}
	if first.StreamID() == second.StreamID() {
		t.Fatal("expected distinct stream IDs")
	}

	// Первый стрим намеренно не закрываем — он остаётся открытым.
	if _, err := first.Write([]byte("from first")); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if _, err := second.Write([]byte("from second")); err != nil {
		t.Fatalf("write second: %v", err)
	}

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case m := <-msgCh:
			seen[string(m.Data)] = true
		case <-deadline:
			t.Fatalf("timeout, got only %v", seen)
		}
	}
}
