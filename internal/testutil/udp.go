package testutil

import (
	"net"
	"testing"

	"github.com/quic-go/quic-go"
)

// ListenUDP открывает эфемерный UDP-сокет на 127.0.0.1.
func ListenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// NewTransport оборачивает локальный UDP-сокет в quic.Transport и закрывает его в cleanup.
func NewTransport(t *testing.T) *quic.Transport {
	t.Helper()
	tr := &quic.Transport{Conn: ListenUDP(t)}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}
