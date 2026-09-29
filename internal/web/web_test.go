package web_test

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"quic_server_go/internal/tlsconfig"
	"quic_server_go/internal/web"

	"github.com/quic-go/quic-go/http3"
)

// Один и тот же хендлер обслуживает HTTP/3 поверх UDP и HTTP/2 поверх TCP.
func TestServesHTTP3AndHTTP2(t *testing.T) {
	cert, err := tlsconfig.NewCertificate()
	if err != nil {
		t.Fatalf("cert: %v", err)
	}

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = udpConn.Close() })

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = tcpLn.Close() })

	servers := web.New(cert, udpConn.LocalAddr().String())
	t.Cleanup(func() { _ = servers.Close() })

	go func() { _ = servers.ServeUDP(udpConn) }()
	go func() { _ = servers.ServeTCP(tcpLn) }()

	t.Run("http3", func(t *testing.T) {
		tr := &http3.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
		}
		defer tr.Close()

		proto, body := get(t, &http.Client{Transport: tr, Timeout: 10 * time.Second},
			"https://"+udpConn.LocalAddr().String()+"/")
		if proto != "HTTP/3.0" {
			t.Fatalf("proto: got %q, want HTTP/3.0", proto)
		}
		if !strings.Contains(body, "HTTP/3.0") {
			t.Fatalf("body does not mention protocol: %q", body)
		}
	})

	t.Run("http2 fallback", func(t *testing.T) {
		tr := &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			ForceAttemptHTTP2: true,
		}
		defer tr.CloseIdleConnections()

		proto, _ := get(t, &http.Client{Transport: tr, Timeout: 10 * time.Second},
			"https://"+tcpLn.Addr().String()+"/")
		if proto != "HTTP/2.0" {
			t.Fatalf("proto: got %q, want HTTP/2.0", proto)
		}
	})
}

func get(t *testing.T, c *http.Client, url string) (proto, body string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	return resp.Proto, string(data)
}
