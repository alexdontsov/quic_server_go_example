// Один хендлер, доступный одновременно по HTTP/3 (UDP) и HTTP/2 + HTTP/1.1 (TCP).
// Так в проде реализуют fallback на случай, когда UDP заблокирован.
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"quic_server_go/internal/tlsconfig"
	"quic_server_go/internal/web"
)

const addr = "127.0.0.1:4433"

func main() {
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")

	cert, err := tlsconfig.NewCertificate()
	if err != nil {
		log.Fatalf("tls: %v", err)
	}

	servers := web.New(cert, addr)
	defer servers.Close()

	udpConn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("listen udp: %v", err)
	}
	defer udpConn.Close()

	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen tcp: %v", err)
	}
	defer tcpLn.Close()

	go func() {
		if err := servers.ServeTCP(tcpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("tcp server: %v", err)
		}
	}()

	fmt.Printf("HTTP/1.1 + HTTP/2 on tcp://%s\n", addr)
	fmt.Printf("HTTP/3            on udp://%s\n\n", addr)
	fmt.Println("Проверка:")
	fmt.Printf("  curl --http3-only -k https://%s/\n", addr)
	fmt.Printf("  curl --http2      -k https://%s/\n", addr)
	fmt.Printf("  curl --http1.1    -k https://%s/\n", addr)

	if err := servers.ServeUDP(udpConn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("quic server: %v", err)
	}
}
