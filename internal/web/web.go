// Package web поднимает один и тот же HTTP-хендлер поверх HTTP/3 (QUIC/UDP)
// и HTTP/2 + HTTP/1.1 (TCP) — так в проде обычно делают fallback,
// когда UDP режут файрволы.
package web

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// Handler возвращает мультиплексор, который сообщает, по какой версии
// протокола пришёл запрос. Если h3 не nil, в ответ добавляется заголовок
// Alt-Svc — по нему браузер узнаёт, что тот же ресурс доступен по HTTP/3.
func Handler(h3 *http3.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if h3 != nil {
			// Ошибку игнорируем: Alt-Svc недоступен, пока порт не определён.
			_ = h3.SetQUICHeaders(w.Header())
		}
		fmt.Fprintf(w, "Hello from %s! path=%s\n", r.Proto, r.URL.Path)
	})
	return mux
}

// Servers держит пару серверов, обслуживающих один хендлер.
type Servers struct {
	H3  *http3.Server
	TCP *http.Server
}

// New собирает пару серверов с общим сертификатом.
// ALPN для HTTP/3 выставляет сам http3.Server, для TCP указываем h2 и http/1.1.
//
// Allow0RTT здесь работает: http3.Server внутри поднимает листенер через
// quic.ListenEarly, а не quic.Listen.
func New(cert tls.Certificate, addr string) *Servers {
	h3 := &http3.Server{
		Addr:      addr,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		QUICConfig: &quic.Config{
			Allow0RTT: true,
		},
	}
	handler := Handler(h3)
	h3.Handler = handler

	// TLS 1.3 — как и в QUIC, чтобы сравнение транспортов было честным.
	tcp := &http.Server{
		Addr:    addr,
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h2", "http/1.1"},
		},
	}

	return &Servers{H3: h3, TCP: tcp}
}

// ServeUDP обслуживает HTTP/3 на готовом UDP-сокете.
func (s *Servers) ServeUDP(conn net.PacketConn) error {
	return s.H3.Serve(conn)
}

// ServeTCP обслуживает HTTP/2 и HTTP/1.1 на готовом TCP-листенере.
func (s *Servers) ServeTCP(ln net.Listener) error {
	return s.TCP.ServeTLS(ln, "", "")
}

// Close останавливает оба сервера.
func (s *Servers) Close() error {
	h3Err := s.H3.Close()
	tcpErr := s.TCP.Close()
	if h3Err != nil {
		return h3Err
	}
	return tcpErr
}
