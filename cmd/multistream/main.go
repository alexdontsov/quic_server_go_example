// Демонстрация независимости стримов: тяжёлая заливка в одном стриме
// не мешает коротким управляющим сообщениям в другом.
//
// Флаг -tcp прогоняет тот же сценарий поверх одного TCP+TLS-соединения,
// в которое оба канала упакованы кадрами (как в HTTP/2). Разница между
// режимами видна под потерями пакетов, см. README.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quic_server_go/internal/alpn"
	"quic_server_go/internal/client"
	"quic_server_go/internal/gapstats"
	"quic_server_go/internal/server"
	"quic_server_go/internal/tlsconfig"

	"github.com/quic-go/quic-go"
)

const (
	bulkChunk     = 64 << 10
	bulkLimit     = 4 << 30 // предохранитель, чтобы демо не крутилось вечно
	controlEvery  = 25 * time.Millisecond
	controlRounds = 40
	settleTime    = 500 * time.Millisecond // ждём хвост данных, уже находящихся в пути
)

// Типы кадров для TCP-режима.
const (
	frameBulk    byte = 0
	frameControl byte = 1
)

func main() {
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")

	useTCP := flag.Bool("tcp", false, "run the same scenario over a single TCP+TLS connection")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	obs := newObserver()
	var err error
	if *useTCP {
		err = runTCP(ctx, obs)
	} else {
		err = runQUIC(ctx, obs)
	}
	if err != nil {
		log.Fatalf("multistream demo: %v", err)
	}
	obs.report()
}

// observer живёт на стороне сервера: считает байты заливки и фиксирует
// моменты прихода управляющих сообщений.
type observer struct {
	transport string
	start     time.Time
	bulkBytes atomic.Int64
	gaps      gapstats.Recorder
}

func newObserver() *observer {
	return &observer{start: time.Now()}
}

func (o *observer) onBulk(n int) { o.bulkBytes.Add(int64(n)) }

// onControl получает всё, что пришло в управляющем канале за одно чтение.
// У стрима нет границ сообщений: задержанный и следующий за ним «пинг»
// могут приехать вместе, поэтому сообщения разделены переводом строки.
func (o *observer) onControl(data []byte) {
	now := time.Since(o.start)
	for msg := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		o.gaps.Record(now)
		fmt.Printf("[server] control %-8s at %7.1f ms | bulk received: %7.2f MiB\n",
			msg, millis(now), mib(o.bulkBytes.Load()))
	}
}

func (o *observer) report() {
	s := o.gaps.Summary()
	fmt.Printf("\ntransport: %s\n", o.transport)
	fmt.Printf("bulk delivered: %.2f MiB\n", mib(o.bulkBytes.Load()))
	fmt.Printf("control messages: %d/%d, send interval: %d ms\n", s.Count, controlRounds, controlEvery.Milliseconds())
	fmt.Println(s)
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func mib(n int64) float64            { return float64(n) / (1 << 20) }

// drive запускает две конкурирующие «нагрузки»: непрерывную заливку и
// короткие сообщения с фиксированным интервалом. Транспорт задаётся снаружи.
func drive(sendBulk, sendControl func([]byte) error) {
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)

	// Тяжёлый стрим: старается занять весь канал.
	go func() {
		defer wg.Done()
		chunk := make([]byte, bulkChunk)
		for sent := 0; sent < bulkLimit; sent += len(chunk) {
			select {
			case <-done:
				return
			default:
			}
			if err := sendBulk(chunk); err != nil {
				return
			}
		}
	}()

	// Лёгкий стрим: короткие сообщения с фиксированным интервалом.
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 1; i <= controlRounds; i++ {
			if err := sendControl(fmt.Appendf(nil, "ping #%02d\n", i)); err != nil {
				return
			}
			time.Sleep(controlEvery)
		}
	}()

	wg.Wait()
	time.Sleep(settleTime)
}

// runQUIC: два стрима одного QUIC-соединения.
func runQUIC(ctx context.Context, obs *observer) error {
	obs.transport = "QUIC, two streams in one connection"

	tlsServer, err := tlsconfig.NewServer(alpn.Protocol)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}

	// Идентификаторы стримов станут известны после их открытия;
	// сервер использует их, чтобы отличать управляющий канал от заливки.
	var controlID, bulkID atomic.Int64
	controlID.Store(-1)
	bulkID.Store(-1)

	srv, err := server.Listen(server.Config{
		Addr:      "127.0.0.1:0",
		TLSConfig: tlsServer,
		OnMessage: func(m server.Message) {
			switch int64(m.StreamID) {
			case bulkID.Load():
				obs.onBulk(len(m.Data))
			case controlID.Load():
				obs.onControl(m.Data)
			}
		},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	tr := &quic.Transport{Conn: udpConn}
	defer tr.Close()

	conn, err := client.Dial(ctx, tr, srv.Addr().String(), tlsconfig.NewInsecureClient(alpn.Protocol), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Основной стрим сессии играет роль управляющего канала.
	controlID.Store(int64(conn.StreamID()))

	bulk, err := conn.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer bulk.Close()
	bulkID.Store(int64(bulk.StreamID()))

	fmt.Printf("control stream id=%d, bulk stream id=%d\n\n", controlID.Load(), bulkID.Load())

	drive(
		func(chunk []byte) error { _, err := bulk.Write(chunk); return err },
		conn.Send,
	)
	return nil
}

// runTCP: один TCP+TLS-поток байтов, оба канала упакованы в кадры
// [тип:1][длина:4][данные]. Именно так живут стримы HTTP/2.
func runTCP(ctx context.Context, obs *observer) error {
	obs.transport = "TCP+TLS, two channels framed into one connection"

	tlsServer, err := tlsconfig.NewServer(alpn.Protocol)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsServer)
	if err != nil {
		return fmt.Errorf("listen tcp: %w", err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() { serverErr <- serveTCP(ln, obs) }()

	dialer := &tls.Dialer{Config: tlsconfig.NewInsecureClient(alpn.Protocol)}
	rawConn, err := dialer.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		return fmt.Errorf("dial tcp: %w", err)
	}

	// Один сокет — одна очередь записи: кадры разных каналов встают в неё по очереди.
	var mu sync.Mutex
	writeFrame := func(kind byte, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		var hdr [5]byte
		hdr[0] = kind
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
		if _, err := rawConn.Write(hdr[:]); err != nil {
			return err
		}
		_, err := rawConn.Write(payload)
		return err
	}

	fmt.Printf("control channel: frame type %d, bulk channel: frame type %d\n\n", frameControl, frameBulk)

	drive(
		func(chunk []byte) error { return writeFrame(frameBulk, chunk) },
		func(msg []byte) error { return writeFrame(frameControl, msg) },
	)

	_ = rawConn.Close()
	if err := <-serverErr; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("tcp server: %w", err)
	}
	return nil
}

// serveTCP принимает одно соединение и разбирает кадры.
func serveTCP(ln net.Listener, obs *observer) error {
	conn, err := ln.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()

	r := bufio.NewReaderSize(conn, bulkChunk)
	payload := make([]byte, bulkChunk)
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		if int(n) > len(payload) {
			return fmt.Errorf("frame too large: %d bytes", n)
		}
		if _, err := io.ReadFull(r, payload[:n]); err != nil {
			return err
		}
		switch hdr[0] {
		case frameBulk:
			obs.onBulk(int(n))
		case frameControl:
			obs.onControl(payload[:n])
		default:
			return fmt.Errorf("unknown frame type %d", hdr[0])
		}
	}
}
