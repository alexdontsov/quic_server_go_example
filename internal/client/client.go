// Package client предоставляет QUIC-клиент с поддержкой миграции соединения.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// Conn — установленная QUIC-сессия с одним открытым двунаправленным стримом.
type Conn struct {
	mu     sync.Mutex
	conn   *quic.Conn
	stream *quic.Stream
}

// Dial устанавливает QUIC-соединение к serverAddr через tr и открывает один стрим.
// Вызывающий сохраняет владение tr и должен закрыть его после Conn.Close.
func Dial(ctx context.Context, tr *quic.Transport, serverAddr string, tlsConf *tls.Config, quicConf *quic.Config) (*Conn, error) {
	if tr == nil {
		return nil, errors.New("transport is required")
	}
	if tlsConf == nil {
		return nil, errors.New("tls config is required")
	}
	if len(tlsConf.NextProtos) == 0 {
		return nil, errors.New("tls config NextProtos (ALPN) is required")
	}

	udpAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve server address: %w", err)
	}

	qconn, err := tr.Dial(ctx, udpAddr, tlsConf, quicConf)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	stream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		_ = qconn.CloseWithError(0, "open stream failed")
		return nil, fmt.Errorf("open stream: %w", err)
	}

	return &Conn{
		conn:   qconn,
		stream: stream,
	}, nil
}

// Send пишет сообщение в стрим сессии.
func (c *Conn) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream == nil {
		return errors.New("connection closed")
	}
	_, err := c.stream.Write(data)
	return err
}

// Migrate проверяет новый сетевой путь на next и переключает соединение на него.
// Вызывающий сохраняет владение next и должен закрыть его после Conn.Close.
func (c *Conn) Migrate(ctx context.Context, next *quic.Transport) error {
	if next == nil {
		return errors.New("transport is required")
	}

	c.mu.Lock()
	qconn := c.conn
	c.mu.Unlock()

	if qconn == nil {
		return errors.New("connection closed")
	}

	path, err := qconn.AddPath(next)
	if err != nil {
		return fmt.Errorf("add path: %w", err)
	}

	probeCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
	}

	if err := path.Probe(probeCtx); err != nil {
		_ = path.Close()
		return fmt.Errorf("probe path: %w", err)
	}
	if err := path.Switch(); err != nil {
		_ = path.Close()
		return fmt.Errorf("switch path: %w", err)
	}

	// Switch только ставит миграцию в очередь; LocalAddr обновится, когда
	// event loop соединения применит новый путь. Ждём активного нового пути.
	want := next.Conn.LocalAddr().String()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for qconn.LocalAddr().String() != want {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for path switch: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

// LocalAddr возвращает локальный адрес активного пути.
func (c *Conn) LocalAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr()
}

// RemoteAddr возвращает адрес удалённого сервера.
func (c *Conn) RemoteAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.RemoteAddr()
}

// Close закрывает стрим и QUIC-соединение.
// Transport'ы не закрываются — ими владеет вызывающий код.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	if c.stream != nil {
		if err := c.stream.Close(); err != nil {
			errs = append(errs, err)
		}
		c.stream = nil
	}
	if c.conn != nil {
		if err := c.conn.CloseWithError(0, "bye"); err != nil {
			errs = append(errs, err)
		}
		c.conn = nil
	}
	return errors.Join(errs...)
}
