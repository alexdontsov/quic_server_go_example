package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"time"

	"quic_server_go/internal/alpn"
	"quic_server_go/internal/client"
	"quic_server_go/internal/server"
	"quic_server_go/internal/tlsconfig"

	"github.com/quic-go/quic-go"
)

const listenAddr = "127.0.0.1:4242"

func main() {
	// Для localhost обычно не нужны буферы на 7 MiB — глушим разовое предупреждение quic-go.
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tlsServer, err := tlsconfig.NewServer(alpn.Protocol)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}

	srv, err := server.Listen(server.Config{
		Addr:      listenAddr,
		TLSConfig: tlsServer,
		OnMessage: func(remote net.Addr, data []byte) {
			fmt.Printf("[server] received: %s | client addr: %s\n", data, remote)
		},
		Logger: logger,
	})
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()

	fmt.Printf("[server] listening on udp://%s\n", srv.Addr())

	if err := runDemo(ctx, srv.Addr().String()); err != nil {
		log.Fatalf("demo: %v", err)
	}

	cancel()
	srv.Close()
	<-errCh
}

func runDemo(ctx context.Context, serverAddr string) error {
	tlsClient := tlsconfig.NewInsecureClient(alpn.Protocol)

	// Порт 5001 имитирует Wi-Fi.
	udp1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5001})
	if err != nil {
		return fmt.Errorf("listen udp 5001: %w", err)
	}
	tr1 := &quic.Transport{Conn: udp1}
	defer tr1.Close()

	fmt.Println("[client] connecting via port 5001 (Wi-Fi)...")
	conn, err := client.Dial(ctx, tr1, serverAddr, tlsClient, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.Send([]byte("hello from Wi-Fi")); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	fmt.Println("[client] migrating to port 7007 (LTE)...")

	// Порт 7007 имитирует LTE.
	udp2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7007})
	if err != nil {
		return fmt.Errorf("listen udp 7007: %w", err)
	}
	tr2 := &quic.Transport{Conn: udp2}
	defer tr2.Close()

	migrateCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := conn.Migrate(migrateCtx, tr2); err != nil {
		return err
	}
	fmt.Printf("[client] migrated, local addr: %s\n", conn.LocalAddr())

	// Тот же стрим — без переоткрытия сессии.
	if err := conn.Send([]byte("hello from LTE; connection kept")); err != nil {
		return fmt.Errorf("send after migrate: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}
