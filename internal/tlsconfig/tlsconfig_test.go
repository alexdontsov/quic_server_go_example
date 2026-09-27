package tlsconfig_test

import (
	"crypto/tls"
	"testing"

	"quic_server_go/internal/tlsconfig"
)

func TestNewServer(t *testing.T) {
	t.Parallel()

	cfg, err := tlsconfig.NewServer("test-alpn")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates: got %d, want 1", len(cfg.Certificates))
	}
	if got := cfg.NextProtos; len(got) != 1 || got[0] != "test-alpn" {
		t.Fatalf("NextProtos: got %v, want [test-alpn]", got)
	}
}

func TestNewInsecureClient(t *testing.T) {
	t.Parallel()

	cfg := tlsconfig.NewInsecureClient("test-alpn")
	if !cfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify")
	}
	if got := cfg.NextProtos; len(got) != 1 || got[0] != "test-alpn" {
		t.Fatalf("NextProtos: got %v, want [test-alpn]", got)
	}

	// Sanity: config is usable as a TLS client config value.
	_ = (*tls.Config)(cfg)
}
