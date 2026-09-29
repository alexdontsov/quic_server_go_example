// Package qlogging подключает qlog-трассировку, если задана переменная QLOGDIR.
package qlogging

import (
	"os"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
)

// Config возвращает quic.Config с qlog-трассировщиком, когда выставлена
// переменная окружения QLOGDIR, и nil в остальных случаях.
// Файлы вида <odcid>_<perspective>.sqlog открываются на qvis.quictools.info.
func Config() *quic.Config {
	if os.Getenv("QLOGDIR") == "" {
		return nil
	}
	return &quic.Config{Tracer: qlog.DefaultConnectionTracer}
}
