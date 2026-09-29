// Демонстрация Connection Migration: клиент меняет IP-адрес и UDP-порт
// посреди живой сессии, а соединение и стрим продолжают работать.
//
// Клиент и сервер живут в одном процессе на loopback. Второй путь привязан
// к 127.0.0.2 — на Linux весь 127.0.0.0/8 висит на lo, так что меняется
// не только порт, но и IP. На macOS 127.0.0.2 нужно добавить руками:
// sudo ifconfig lo0 alias 127.0.0.2 up
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"quic_server_go/internal/alpn"
	"quic_server_go/internal/client"
	"quic_server_go/internal/qlogging"
	"quic_server_go/internal/server"
	"quic_server_go/internal/tlsconfig"

	"github.com/quic-go/quic-go"
)

const listenAddr = "127.0.0.1:4242"

// Два «интерфейса» клиента: условный Wi-Fi и условный LTE.
var (
	wifiAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5001}
	lteAddr  = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 7007}
)

func main() {
	// Для localhost не нужны буферы на 7 MiB — глушим разовое предупреждение quic-go.
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")

	// Один формат вывода для сервера и клиента; метку времени убираем — в демо она только шумит.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	serverLog := logger.With("side", "server")
	clientLog := logger.With("side", "client")

	tlsServer, err := tlsconfig.NewServer(alpn.Protocol)
	if err != nil {
		fatal(logger, "tls", err)
	}

	srv, err := server.Listen(server.Config{
		Addr:       listenAddr,
		TLSConfig:  tlsServer,
		QUICConfig: qlogging.Config(),
		OnMessage: func(m server.Message) {
			serverLog.Info("message received", "data", string(m.Data), "remote", m.Remote)
		},
		Logger: serverLog,
	})
	if err != nil {
		fatal(logger, "listen", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()

	serverLog.Info("listening", "addr", "udp://"+srv.Addr().String())

	if err := runDemo(ctx, clientLog, srv.Addr().String()); err != nil {
		fatal(logger, "demo", err)
	}

	cancel()
	srv.Close()
	<-errCh
}

func runDemo(ctx context.Context, log *slog.Logger, serverAddr string) error {
	tlsClient := tlsconfig.NewInsecureClient(alpn.Protocol)

	tr1, err := newTransport(wifiAddr)
	if err != nil {
		return err
	}
	defer tr1.Close()

	log.Info("connecting", "via", wifiAddr, "as", "Wi-Fi")
	conn, err := client.Dial(ctx, tr1, serverAddr, tlsClient, qlogging.Config())
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.Send([]byte("hello from Wi-Fi")); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	tr2, err := newTransport(lteAddr)
	if err != nil {
		return err
	}
	defer tr2.Close()

	log.Info("migrating", "to", lteAddr, "as", "LTE")
	migrateCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := conn.Migrate(migrateCtx, tr2); err != nil {
		return err
	}
	// Адрес берём у транспорта: conn.LocalAddr() какое-то время ещё будет отдавать старый путь.
	log.Info("migrated", "local", tr2.Conn.LocalAddr())

	// Switch применяется event loop'ом асинхронно; даём ему итерацию,
	// чтобы следующее сообщение наверняка ушло уже по новому пути.
	time.Sleep(100 * time.Millisecond)

	// Тот же стрим — без нового соединения и повторного TLS-рукопожатия.
	if err := conn.Send([]byte("hello from LTE; connection kept")); err != nil {
		return fmt.Errorf("send after migrate: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

func newTransport(addr *net.UDPAddr) (*quic.Transport, error) {
	udpConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %s: %w", addr, err)
	}
	return &quic.Transport{Conn: udpConn}, nil
}

func fatal(log *slog.Logger, stage string, err error) {
	log.Error(stage+" failed", "err", err)
	os.Exit(1)
}
