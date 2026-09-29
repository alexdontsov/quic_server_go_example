# QUIC Demo (Go)

Учебный проект к статье [«Connection Migration в QUIC на Go: что показывает qlog и где quic-go отступает от RFC»](doc/article.md).

Три небольших демо на [quic-go](https://github.com/quic-go/quic-go), показывающих ключевые свойства QUIC:

| Демо | Что показывает |
|------|----------------|
| `cmd/migration` | **Connection Migration** — смена IP и UDP-порта клиента без разрыва соединения |
| `cmd/multistream` | **Независимость стримов** — тяжёлая заливка не задерживает управляющий канал; флаг `-tcp` прогоняет тот же сценарий поверх одного TCP-соединения для сравнения |
| `cmd/http3` | **HTTP/3 + fallback** — один хендлер по UDP (h3) и TCP (h2, http/1.1) |

## Требования

- Go **1.24+** (собрано и проверено на 1.26)
- Linux / macOS
- Свободные UDP-порты `4242`, `5001`, `7007` (миграция) и `4433` (HTTP/3)
- Для миграции второй путь привязывается к `127.0.0.2`. На Linux этот адрес есть всегда; на macOS его нужно добавить: `sudo ifconfig lo0 alias 127.0.0.2 up`

## Запуск

```bash
go test ./...
go run ./cmd/migration
go run ./cmd/multistream
go run ./cmd/multistream -tcp
go run ./cmd/http3
```

### Connection Migration

```console
$ go run ./cmd/migration
level=INFO msg=listening side=server addr=udp://127.0.0.1:4242
level=INFO msg=connecting side=client via=127.0.0.1:5001 as=Wi-Fi
level=INFO msg="client connected" side=server remote=127.0.0.1:5001
level=INFO msg="message received" side=server data="hello from Wi-Fi" remote=127.0.0.1:5001
level=INFO msg=migrating side=client to=127.0.0.2:7007 as=LTE
level=INFO msg=migrated side=client local=127.0.0.2:7007
level=INFO msg="message received" side=server data="hello from LTE; connection kept" remote=127.0.0.2:7007
```

Соединение не пересоздаётся: меняется сетевой путь, сервер узнаёт соединение по Connection ID, которые сам выдал клиенту при рукопожатии. Какие именно CID при этом использует quic-go — см. раздел про qlog ниже и в статье.

### Независимость стримов

```console
$ go run ./cmd/multistream
control stream id=0, bulk stream id=4

[server] control ping #01 at     2.5 ms | bulk received:    0.00 MiB
[server] control ping #02 at    27.4 ms | bulk received:   15.01 MiB
...
[server] control ping #40 at   979.8 ms | bulk received:  672.14 MiB

transport: QUIC, two streams in one connection
bulk delivered: 689.06 MiB
control messages: 40/40, send interval: 25 ms
gap p50 / p95 / max: 25.1 / 25.3 / 25.3 ms
```

На чистом loopback потерь нет, и QUIC с TCP (`-tcp`) ведут себя одинаково хорошо. Разница появляется под потерями. Чтобы не трогать `lo` хост-системы, эмулируем плохую сеть в отдельном network namespace — без `sudo`, откатывать ничего не нужно:

```bash
go build -o /tmp/multistream ./cmd/multistream

unshare -Urn sh -c 'ip link set lo up &&
  tc qdisc add dev lo root netem delay 20ms loss 2% &&
  /tmp/multistream'

unshare -Urn sh -c 'ip link set lo up &&
  tc qdisc add dev lo root netem delay 20ms loss 2% &&
  /tmp/multistream -tcp'
```

| Вариант | p50 | p95 | max |
| :--- | ---: | ---: | ---: |
| QUIC, loopback | 25.1 мс | 25.3 мс | 25.3 мс |
| TCP, loopback | 26.1 мс | 26.1 мс | 26.2 мс |
| QUIC, 2 % потерь + 40 мс RTT | 26.7 мс | 40.2 мс | 40.2 мс |
| TCP, 2 % потерь + 40 мс RTT | 120.6 мс | 333.8 мс | 363.6 мс |

Пропускную способность между вариантами сравнивать не стоит: `netem` теряет пакеты, а не байты, а TCP на loopback шлёт сегменты до 64 КБ против ~1,2 КБ у QUIC.

### HTTP/3 и fallback

```bash
go run ./cmd/http3 &
curl -s  --http2   -k https://127.0.0.1:4433/     # Hello from HTTP/2.0!
curl -s  --http1.1 -k https://127.0.0.1:4433/     # Hello from HTTP/1.1!
curl -sI --http2   -k https://127.0.0.1:4433/ | grep -i alt-svc
```

Для `curl --http3-only` нужна сборка curl с HTTP/3-бэкендом (ngtcp2 или quiche): `curl --version | grep -o HTTP3`. Системный curl обычно без неё — HTTP/3 проверяется Go-тестом через `http3.Transport`. Браузеры с самоподписанным сертификатом по `Alt-Svc` на HTTP/3 не переходят.

## Отладка через qlog

```bash
QLOGDIR=/tmp/qlogs go run ./cmd/migration

# фреймы валидации пути: по два, потому что валидация двусторонняя
grep -o 'path_challenge\|path_response' /tmp/qlogs/*_client.sqlog | sort | uniq -c

# с какими Destination CID уходят пакеты (формат .sqlog — JSON Text Sequences, отсюда --seq)
jq -r --seq 'select((.name // "") | test("packet_(sent|received)"))
  | select([.data.frames[]?.frame_type] | any(IN("path_challenge","path_response","stream")))
  | "\(.time|floor)ms\t\(.name|sub("transport:";""))\t\(.data.header.dcid)\t\([.data.frames[].frame_type]|join(","))"' \
  /tmp/qlogs/*_client.sqlog
```

Проба нового пути уходит с новым CID, а данные после `Switch()` — со старым (quic-go [#5236](https://github.com/quic-go/quic-go/issues/5236)). Файлы `.sqlog` также открываются в [qvis](https://qvis.quictools.info/).

## Структура

```text
.
├── cmd/
│   ├── migration/          # демо Connection Migration
│   ├── multistream/        # демо независимости стримов (QUIC и -tcp)
│   └── http3/              # HTTP/3 + HTTP/2 fallback
├── doc/article.md          # текст статьи
├── internal/
│   ├── alpn/               # общий ALPN
│   ├── tlsconfig/          # self-signed TLS в памяти
│   ├── server/             # QUIC-сервер (Accept → AcceptStream → OnMessage)
│   ├── client/             # Dial / Send / OpenStream / Migrate
│   ├── web/                # HTTP/3 и HTTP/2 поверх одного хендлера
│   ├── gapstats/           # перцентили интервалов между сообщениями
│   ├── qlogging/           # qlog по переменной QLOGDIR
│   └── testutil/           # хелперы для тестов
├── go.mod
└── go.sum
```

## Тесты

```bash
go test -count=1 -v ./...
go test -race ./...
```

Покрыто: генерация TLS и проверка ALPN, приём сообщений сервером, параллельные стримы, `Dial`/`OpenStream`, connection migration, HTTP/3 и HTTP/2 через один хендлер, статистика интервалов.

## Замечания

- Демо выставляют `QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING`, чтобы не пугать предупреждением quic-go о размере UDP-буфера. Для реальной нагрузки лучше поднять лимиты ядра, см. [UDP Buffer Sizes](https://github.com/quic-go/quic-go/wiki/UDP-Buffer-Sizes).
- Self-signed сертификат и `InsecureSkipVerify` — только для localhost.
- Runtime-сообщения в коде на английском, комментарии — на русском.
- API quic-go менялся между версиями; код проверен на `github.com/quic-go/quic-go` v0.61.0 и v0.63.0.

Пример к статье, не production-ready библиотека.
