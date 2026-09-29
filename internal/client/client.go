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

// Send пишет сообщение в основной стрим сессии.
func (c *Conn) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream == nil {
		return errors.New("connection closed")
	}
	_, err := c.stream.Write(data)
	return err
}

// OpenStream открывает дополнительный двунаправленный стрим в том же соединении.
// Стримы независимы друг от друга: у каждого свои offset'ы и flow control.
func (c *Conn) OpenStream(ctx context.Context) (*quic.Stream, error) {
	c.mu.Lock()
	qconn := c.conn
	c.mu.Unlock()

	if qconn == nil {
		return nil, errors.New("connection closed")
	}
	stream, err := qconn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	return stream, nil
}

// StreamID возвращает идентификатор основного стрима сессии.
func (c *Conn) StreamID() quic.StreamID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stream.StreamID()
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

	// Шаг 1: регистрируем новый путь. Пакеты по нему ещё не идут.
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

	// Шаг 2: валидация пути через PATH_CHALLENGE / PATH_RESPONSE.
	// Без неё Switch вернёт quic.ErrPathNotValidated.
	if err := path.Probe(probeCtx); err != nil {
		_ = path.Close()
		return fmt.Errorf("probe path: %w", err)
	}
	// Шаг 3: переключение трафика на новый путь.
	//
	// Switch не применяет переключение немедленно, а ставит его в очередь:
	// новый путь активирует event loop соединения на следующей итерации.
	// Отсюда два следствия. Во-первых, пакет, который event loop уже
	// собирал в момент вызова, может уйти ещё по старому пути — данные
	// не потеряются, но «первое сообщение с нового адреса» гарантировать
	// нельзя. Во-вторых, LocalAddr какое-то время будет отдавать старый
	// адрес, и опрашивать его в цикле нельзя: quic-go меняет нижележащий
	// сокет без синхронизации с LocalAddr, и race detector справедливо
	// ругается. Актуальный адрес нового пути — next.Conn.LocalAddr().
	if err := path.Switch(); err != nil {
		_ = path.Close()
		return fmt.Errorf("switch path: %w", err)
	}
	return nil
}

// LocalAddr возвращает локальный адрес активного пути.
//
// Сразу после Migrate значение может какое-то время отставать: quic-go
// подменяет нижележащий сокет асинхронно. Если нужен гарантированно
// актуальный адрес, берите его у Transport, на который мигрировали.
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
