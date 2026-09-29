# QUIC Connection Migration Demo (Go)

Учебный проект к статье [«Погружение в QUIC: как устроен современный протокол с примерами на Go»](doc/article.md).

Демонстрирует работу **QUIC** на Go через [quic-go](https://github.com/quic-go/quic-go): минимальный сервер, клиент и **Connection Migration** — бесшовная смена UDP-порта (имитация Wi‑Fi → LTE) без переоткрытия сессии и TLS-рукопожатия.

## Что показывает демо

1. Сервер слушает UDP `127.0.0.1:4242`.
2. Клиент подключается с порта `5001` («Wi‑Fi») и отправляет сообщение.
3. Клиент мигрирует на порт `7007` («LTE») через `AddPath` → `Probe` → `Switch`.
4. То же соединение и тот же стрим продолжают работать; на сервере меняется только `RemoteAddr`.

## Требования

- Go **1.26+**
- Linux / macOS (UDP localhost)

## Быстрый старт

```bash
go test ./...
go run ./cmd
```

Ожидаемый вывод (сокращённо):

```text
[server] listening on udp://127.0.0.1:4242
[client] connecting via port 5001 (Wi-Fi)...
[server] received: hello from Wi-Fi | client addr: 127.0.0.1:5001
[client] migrating to port 7007 (LTE)...
[client] migrated, local addr: 127.0.0.1:7007
[server] received: hello from LTE; connection kept | client addr: 127.0.0.1:7007
```

Важно: сессия не пересоздаётся — меняется сетевой путь, Connection ID остаётся тем же.

## Структура

```text
.
├── cmd/main.go                 # точка входа: сервер + клиент + migrate
├── doc/article.md              # текст статьи
├── internal/
│   ├── alpn/                   # общий ALPN (`quic-habr-demo`)
│   ├── tlsconfig/              # self-signed TLS в памяти
│   ├── server/                 # QUIC-сервер (Accept → AcceptStream)
│   ├── client/                 # Dial / Send / Migrate
│   └── testutil/               # хелперы для тестов
├── go.mod
└── go.sum
```

## API пакетов (кратко)

| Пакет | Назначение |
|-------|------------|
| `internal/tlsconfig` | `NewServer(alpn)`, `NewInsecureClient(alpn)` |
| `internal/server` | `Listen` + `Serve(ctx)` с колбэком `OnMessage` |
| `internal/client` | `Dial`, `Send`, `Migrate` поверх `quic.Transport` |

Миграция в актуальном quic-go — клиентская операция: нужен контроль над UDP-сокетами через `quic.Transport`, а не «голый» `DialAddr`.

## Тесты

```bash
go test -count=1 -v ./...
```

Покрыто:

- генерация TLS / ALPN;
- приём сообщения сервером;
- dial + send;
- connection migration (`TestConnectionMigration`).

## Замечания

- Демо глушит предупреждение quic-go о размере UDP receive buffer (`QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING`). Для высокой нагрузки на Linux лучше поднять лимиты ядра, см. [UDP Buffer Sizes](https://github.com/quic-go/quic-go/wiki/UDP-Buffer-Sizes).
- Self-signed сертификат и `InsecureSkipVerify` — только для localhost.
- Runtime-сообщения в коде на английском; комментарии — на русском (удобно читать вместе со статьёй).

## Лицензия / контекст

Пример к статье, не production-ready библиотека. Зависимость: `github.com/quic-go/quic-go v0.61.0`.
