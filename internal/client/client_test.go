package client_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"quic_server_go/internal/alpn"
	"quic_server_go/internal/client"
	"quic_server_go/internal/server"
	"quic_server_go/internal/testutil"
	"quic_server_go/internal/tlsconfig"
)

func startTestServer(t *testing.T) (addr string, remotes <-chan string) {
	t.Helper()

	tlsSrv, err := tlsconfig.NewServer(alpn.Protocol)
	if err != nil {
		t.Fatalf("tls: %v", err)
	}

	ch := make(chan string, 8)
	srv, err := server.Listen(server.Config{
		Addr:      "127.0.0.1:0",
		TLSConfig: tlsSrv,
		OnMessage: func(m server.Message) { ch <- m.Remote.String() },
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

	return srv.Addr().String(), ch
}

func TestDialAndSend(t *testing.T) {
	t.Parallel()

	addr, remotes := startTestServer(t)
	tr := testutil.NewTransport(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := client.Dial(ctx, tr, addr, tlsconfig.NewInsecureClient(alpn.Protocol), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.Send([]byte("hello")); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case remote := <-remotes:
		if remote != conn.LocalAddr().String() {
			// Сервер видит UDP-адрес клиента; LocalAddr сессии должен совпадать.
			t.Fatalf("remote=%s local=%s", remote, conn.LocalAddr())
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for server message")
	}
}

func TestOpenStream(t *testing.T) {
	t.Parallel()

	addr, _ := startTestServer(t)
	tr := testutil.NewTransport(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := client.Dial(ctx, tr, addr, tlsconfig.NewInsecureClient(alpn.Protocol), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	extra, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if extra.StreamID() == conn.StreamID() {
		t.Fatalf("expected a distinct stream, got %d twice", extra.StreamID())
	}
}

func TestConnectionMigration(t *testing.T) {
	t.Parallel()

	addr, remotes := startTestServer(t)
	tr1 := testutil.NewTransport(t)
	tr2 := testutil.NewTransport(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := client.Dial(ctx, tr1, addr, tlsconfig.NewInsecureClient(alpn.Protocol), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	path1 := tr1.Conn.LocalAddr().String()
	if err := conn.Send([]byte("on path 1")); err != nil {
		t.Fatalf("send path1: %v", err)
	}
	waitRemote(t, ctx, remotes, path1, nil)

	if err := conn.Migrate(ctx, tr2); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Настоящее доказательство миграции — сервер видит данные с нового
	// адреса, причём в том же самом соединении и стриме.
	//
	// Switch асинхронен: пакет, который event loop собирал в момент
	// переключения, может уйти ещё по старому пути. Поэтому шлём сообщения
	// до тех пор, пока сервер не увидит новый адрес.
	path2 := tr2.Conn.LocalAddr().String()
	waitRemote(t, ctx, remotes, path2, func() error {
		return conn.Send([]byte("on path 2"))
	})
}

// waitRemote ждёт, пока сервер не увидит сообщение с адреса want.
// Перед каждой попыткой вызывает send (если задан).
func waitRemote(t *testing.T, ctx context.Context, remotes <-chan string, want string, send func() error) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if send != nil {
			if err := send(); err != nil {
				t.Fatalf("send to %s: %v", want, err)
			}
		}
		select {
		case got := <-remotes:
			if got == want {
				return
			}
			// Сообщение ушло по старому пути — пробуем ещё раз.
		case <-deadline:
			t.Fatalf("timeout waiting for remote addr %s", want)
		case <-ctx.Done():
			t.Fatalf("context done waiting for remote addr %s: %v", want, ctx.Err())
		}
	}
}
