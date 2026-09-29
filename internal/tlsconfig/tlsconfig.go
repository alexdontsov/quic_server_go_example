// Package tlsconfig собирает tls.Config для демо-сервера и клиента QUIC.
package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// NewCertificate генерирует самоподписанный ECDSA-сертификат для localhost.
// Сертификат живёт в памяти: читателю не нужно ничего готовить заранее.
func NewCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"quic-demo"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal private key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse key pair: %w", err)
	}
	return tlsCert, nil
}

// NewServer возвращает серверный tls.Config со свежим самоподписанным сертификатом.
// ALPN обязателен: QUIC откажется работать с пустым NextProtos.
func NewServer(alpns ...string) (*tls.Config, error) {
	cert, err := NewCertificate()
	if err != nil {
		return nil, err
	}
	return ServerWithCert(cert, alpns...), nil
}

// ServerWithCert собирает серверный tls.Config поверх готового сертификата.
// Нужен, когда один и тот же сертификат обслуживает несколько протоколов (h3 и h2).
func ServerWithCert(cert tls.Certificate, alpns ...string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		NextProtos:   alpns,
	}
}

// NewInsecureClient возвращает клиентский tls.Config без проверки сертификата.
// Только для локальных демо и тестов.
func NewInsecureClient(alpns ...string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         alpns,
	}
}
