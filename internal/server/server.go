// Package server реализует минимальный QUIC-сервер, который отдаёт полезную нагрузку в колбэк.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/quic-go/quic-go"
)

// Message — прикладная порция данных, прочитанная из стрима.
type Message struct {
	// Remote — текущий адрес клиента. После миграции пути он меняется сам собой.
	Remote net.Addr
	// StreamID позволяет отличить один логический канал от другого
	// внутри одного и того же QUIC-соединения.
	StreamID quic.StreamID
	Data     []byte
}

// MessageHandler вызывается для каждого прикладного сообщения на стриме.
type MessageHandler func(Message)

// Config — параметры сервера.
type Config struct {
	Addr       string
	TLSConfig  *tls.Config
	QUICConfig *quic.Config
	OnMessage  MessageHandler
	Logger     *slog.Logger
}

// Server принимает QUIC-соединения и передаёт данные стримов в OnMessage.
type Server struct {
	listener  *quic.Listener
	onMessage MessageHandler
	log       *slog.Logger
}

// Listen поднимает QUIC-listener на cfg.Addr.
func Listen(cfg Config) (*Server, error) {
	if cfg.TLSConfig == nil {
		return nil, errors.New("tls config is required")
	}
	if len(cfg.TLSConfig.NextProtos) == 0 {
		return nil, errors.New("tls config NextProtos (ALPN) is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	ln, err := quic.ListenAddr(cfg.Addr, cfg.TLSConfig, cfg.QUICConfig)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	return &Server{
		listener:  ln,
		onMessage: cfg.OnMessage,
		log:       log,
	}, nil
}

// Addr возвращает адрес listener'а.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Serve принимает соединения, пока не отменят ctx или не закроют listener.
func (s *Server) Serve(ctx context.Context) error {
	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			// Listener закрыт.
			if errors.Is(err, net.ErrClosed) || errors.Is(err, quic.ErrServerClosed) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				s.log.Warn("accept failed", "err", err)
				continue
			}
		}
		go s.handleConn(ctx, conn)
	}
}

// Close останавливает listener.
func (s *Server) Close() error {
	return s.listener.Close()
}

func (s *Server) handleConn(ctx context.Context, conn *quic.Conn) {
	s.log.Info("client connected", "remote", conn.RemoteAddr())
	defer s.log.Info("client disconnected", "remote", conn.RemoteAddr())

	// Каждый стрим обрабатывается независимо: потеря пакета в одном
	// не задерживает чтение остальных.
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.handleStream(stream, conn)
	}
}

func (s *Server) handleStream(stream *quic.Stream, conn *quic.Conn) {
	defer stream.Close()

	buf := make([]byte, 64<<10)
	for {
		n, err := stream.Read(buf)
		if n > 0 && s.onMessage != nil {
			// Копируем, чтобы обработчик мог сохранить слайс.
			data := make([]byte, n)
			copy(data, buf[:n])
			s.onMessage(Message{
				Remote:   conn.RemoteAddr(),
				StreamID: stream.StreamID(),
				Data:     data,
			})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("stream read ended", "err", err)
			}
			return
		}
	}
}
