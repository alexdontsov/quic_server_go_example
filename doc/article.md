# Connection Migration в QUIC на Go: что показывает qlog и где quic-go отступает от RFC

**Содержание**

1. [Проблема TCP: почему появился QUIC](#1-проблема-tcp-почему-появился-quic)
2. [Анатомия QUIC: транспорт в user space](#2-анатомия-quic-транспорт-в-user-space)
3. [Сводная таблица: TCP vs QUIC](#3-сводная-таблица-tcp-vs-quic)
4. [Практика: Connection Migration на Go](#4-практика-connection-migration-на-go)
5. [Практика: стримы не мешают друг другу — на чистом канале и под потерями](#5-практика-стримы-не-мешают-друг-другу--на-чистом-канале-и-под-потерями)
6. [Практика: HTTP/3 и fallback на HTTP/2](#6-практика-http3-и-fallback-на-http2)
7. [Обратная сторона медали: QUIC в production](#7-обратная-сторона-медали-quic-в-production)
8. [Заключение](#8-заключение)

## Введение

HTTP/3 давно вышел из стадии эксперимента: по данным [W3Techs](https://w3techs.com/technologies/details/ce-http3), его поддерживает заметная доля сайтов *(уточнить цифру и дату на момент публикации)*.
Тем не менее для многих бэкенд-разработчиков QUIC до сих пор выглядит как «TCP, засунутый в UDP-пакеты».
На самом деле QUIC (RFC 9000) — полноценный транспортный протокол, спроектированный для современного интернета:
в нём изначально заложены TLS 1.3, независимые стримы без Head-of-Line Blocking между ними, 0-RTT при повторном подключении
и миграция соединения при смене сетевого пути.

В первой части статьи разберём, как устроен QUIC и чем он отличается от TCP.
Во второй — напишем на Go с библиотекой quic-go три небольших демо и проверим заявления протокола на реальном трафике:

* **Connection Migration** — клиент меняет IP и порт посреди живой сессии, а стрим продолжает работать. Заглянем в qlog и увидим, как при этом ведут себя Connection ID — и в одном месте поймаем quic-go на отступлении от RFC.
* **Независимость стримов** — сравним поведение управляющего канала рядом с тяжёлой заливкой в QUIC и в одном TCP-соединении, на чистом loopback и под эмулированными потерями.
* **HTTP/3 с fallback на HTTP/2** — один хендлер на двух транспортах.

0-RTT в демо не используется: для него нужен отдельный сценарий с повторным подключением, и он заслуживает отдельного разбора.

Все примеры собраны в репозитории [quic_server_go_example](https://github.com/alexdontsov/quic_server_go_example).
Код компилируется и покрыт тестами; листинги в статье — фрагменты, полные версии лежат в репозитории.

---

## 1. Проблема TCP: почему появился QUIC

На протяжении четырёх десятилетий TCP оставался главным «грузовиком» интернета.
Он гарантирует доставку, восстанавливает порядок сегментов и регулирует перегрузку сети (congestion control).
Однако TCP проектировался для стационарных машин в стабильных проводных сетях 1980-х.
Сегодня мобильный трафик преобладает, а приложения загружают сотни мелких ресурсов параллельно.
В этих условиях старые механизмы TCP становятся узким местом.

### Задержки на установление соединения

Чтобы клиент и сервер начали обмениваться полезными данными по HTTPS, нужно пройти два последовательных рукопожатия:

```text
  Клиент                                     Сервер
    │                                          │
    ├──────────── TCP SYN ────────────────────>│ ──┐
    │                                          │   │ 1 RTT (TCP Handshake)
    │<─────────── TCP SYN + ACK ───────────────┤ ──┘
    ├──────────── TCP ACK ────────────────────>│
    │                                          │
    ├──────────── TLS ClientHello ────────────>│ ──┐
    │                                          │   │ 1 RTT (TLS 1.3 Handshake)
    │<─────────── TLS ServerHello ─────────────┤ ──┘
    │
    ├──────────── HTTP Request ───────────────>│ ─── первый байт данных приложения
```

1. **TCP Handshake:** 1 RTT (SYN → SYN-ACK → ACK).
2. **TLS 1.3 Handshake:** ещё 1 RTT на согласование ключей и сертификатов.

Итого до первого байта HTTP-запроса проходит **2 полных RTT** (с TLS 1.2 — до 3 RTT). В мобильных сетях или при связи через континент, где RTT достигает 100 мс, пользователь ждёт 200–300 мс только на подготовку канала.

### Head-of-Line Blocking

TCP представляет данные как **единый упорядоченный поток байтов**. Если приложение передаёт через один сокет несколько независимых ресурсов (картинку, CSS и ответ API), сетевой стек ничего не знает об их границах.

```text
Отправлено:  [ Seg 1: CSS ]  [ Seg 2: JS ]  [ Seg 3: Img ]  [ Seg 4: API ]
Сеть:        [ Seg 1: CSS ]  [   ПОТЕРЯ  ]  [ Seg 3: Img ]  [ Seg 4: API ]

Буфер ядра:  [ CSS (OK)   ]  [ ОЖИДАНИЕ  ]  [ Заблокировано ] [ Заблокировано ]
                                 │
                                 └── повторная передача Seg 2
```

Если сегмент №2 с фрагментом JS теряется, ядро получателя удерживает сегменты №3 и №4 в буфере и не отдаёт их приложению, пока №2 не будет доставлен повторно.

**HTTP/2** убрал HoL-блокировку на *уровне приложения* (мультиплексируя запросы во фреймы), но на *транспортном уровне* она осталась: потеря одного TCP-сегмента ставит на паузу все HTTP/2-стримы внутри соединения.

### Привязка к сокету (IP:Port)

Идентификатор TCP-соединения — четвёрка (4-tuple): исходный IP, исходный порт, целевой IP, целевой порт.

Когда смартфон переключается с Wi-Fi на LTE, его IP-адрес меняется, и старое соединение мгновенно умирает. Приложению приходится заново проходить TCP- и TLS-рукопожатия, переоткрывать веб-сокеты и восстанавливать контекст сессии.

---

## 2. Анатомия QUIC: транспорт в user space

Зачем строить новый протокол поверх UDP, который ничего не гарантирует? Как раз поэтому: UDP даёт минимальную обёртку над IP — порты и контрольную сумму — и не навязывает своей логики.

QUIC переносит всю транспортную логику из ядра ОС (kernel space) в пространство пользователя (user space).

```text
  +--------------------------------------------------------+
  |                   HTTP/3 / Application                 |
  +--------------------------------------------------------+
  |                          QUIC                          |
  |  ┌──────────────────┐  ┌────────────────────────────┐  |
  |  │ Streams          │  │ TLS 1.3 Handshake          │  |
  |  ├──────────────────┤  ├────────────────────────────┤  |
  |  │ Loss Recovery    │  │ Connection ID & Migration  │  |
  |  ├──────────────────┤  ├────────────────────────────┤  |
  |  │ Congestion Ctrl  │  │ Flow Control (per stream)  │  |
  |  └──────────────────┘  └────────────────────────────┘  |
  +--------------------------------------------------------+
  |                          UDP                           |
  +--------------------------------------------------------+
  |                           IP                           |
  +--------------------------------------------------------+
```

### Что даёт перенос в user space

1. **Скорость внедрения.** Изменения в TCP требуют обновления ядра на миллионах серверов и промежуточных устройств — это годы (так называемое *ossification*, окостенение сети). QUIC обновляется вместе с релизом браузера или бинарника вашего сервиса на Go.
2. **Защита от вмешательства.** Полезная нагрузка QUIC-пакета зашифрована, включая фреймы `ACK`, а номер пакета в заголовке дополнительно закрыт header protection (RFC 9001). Промежуточным узлам (middleboxes) остаётся видеть только флаги, версию и Connection ID — «подкручивать» окна и ретрансмиссии, как в TCP, они не могут.

### Главные свойства QUIC

#### Объединённое рукопожатие (1-RTT и 0-RTT)

QUIC совмещает транспортное и криптографическое рукопожатие. При первом подключении требуется **1 RTT**. При повторном (resumption) клиент может использовать **0-RTT**, отправляя зашифрованные данные (early data) уже в первом пакете.

```text
  Клиент                                     Сервер
    │                                          │
    ├──────────── Initial: ClientHello ───────>│ ──┐
    │             + QUIC Transport Parameters  │   │ 1 RTT — соединение готово
    │<─────────── Handshake: ServerHello ──────┤ ──┘
    │             + Encrypted Extensions       │
    │                                          │
    ├──────────── Short Header: Stream Data ──>│ ─── первый байт данных приложения
```

0-RTT — не просто «быстрое рукопожатие», и у него есть важное ограничение: early data можно **воспроизвести повторно** (replay). Злоумышленник, перехвативший 0-RTT-пакет, может отправить его серверу ещё раз, и без дополнительных мер сервер выполнит запрос дважды. Поэтому в 0-RTT нельзя без защиты от повторов выполнять неидемпотентные операции — списание денег, создание заказа.

Деталь для практики: в quic-go одного флага `Allow0RTT` в `quic.Config` недостаточно. Сервер должен слушать через `quic.ListenAddrEarly` (или `Transport.ListenEarly`), а клиент — подключаться через `DialEarly`. С обычным `quic.ListenAddr` флаг ничего не делает. В нашем сервере из раздела 4 используется `ListenAddr`, и 0-RTT там нет; а вот `http3.Server` из раздела 6 внутри поднимает листенер через `ListenEarly`, так что там флаг работает.

#### Стримы как сущности первого класса

Внутри одного QUIC-соединения можно открыть тысячи независимых стримов — однонаправленных или двунаправленных. У каждого свой счётчик смещения (offset) и своё окно flow control.

Если пакет с данными стрима №2 потерялся, это **не блокирует** чтение из стримов №1 и №3: стек QUIC отдаст их приложению немедленно.

```text
Stream 1: [ Data Chunk 1A ] ───────────────────────────> App Read Buffer (OK)
Stream 2: [    LOST DATA  ] ── (Retransmitting...) ────> Waiting
Stream 3: [ Data Chunk 3A ] ───────────────────────────> App Read Buffer (OK)
```

#### Connection Migration и Connection ID

QUIC не использует IP-адреса и порты для идентификации соединения. Вместо этого в заголовке каждого пакета передаётся **Connection ID (CID)** — непрозрачный идентификатор длиной от 0 до 20 байт, который каждая сторона генерирует для себя сама. Нулевая длина допустима, только если эндпоинту не нужно демультиплексировать соединения по CID; quic-go по умолчанию использует ненулевые CID.

Важно: CID у соединения не один. В ходе рукопожатия стороны обмениваются фреймами `NEW_CONNECTION_ID`, выдавая друг другу по несколько запасных идентификаторов. Сервер узнаёт «своё» соединение по любому из CID, которые он сам выдал клиенту, — именно поэтому смена IP или порта его не смущает.

Когда клиент решает переехать на новый путь, происходит следующее:

1. Клиент отправляет с нового адреса пакет с фреймом `PATH_CHALLENGE` (8 случайных байт). По RFC 9000 §9.5 такой пакет **обязан** идти с новым CID — иначе наблюдатель в сети смог бы связать старый и новый адрес одного пользователя (linkability).
2. Сервер отвечает `PATH_RESPONSE` с теми же 8 байтами — и, как правило, тут же посылает клиенту **свой** `PATH_CHALLENGE`: он тоже хочет убедиться, что новый адрес клиента реален, а не подделан.
3. Клиент отвечает на встречный вызов. Путь валидирован с обеих сторон; трафик переезжает, соединение и стримы живут дальше.

Валидация двусторонняя — эта деталь пригодится, когда мы будем считать фреймы в qlog.

#### Два разных сценария: NAT rebinding и active migration

Два механизма внешне выглядят одинаково, но путать их не стоит.

**NAT rebinding** — пассивный сценарий. У клиента ничего не менялось, но промежуточный NAT переписал порт (типично после простоя). Сервер видит пакеты знакомого соединения с нового адреса, валидирует путь и продолжает работу. Приложению делать ничего не нужно.

**Active migration** — активный сценарий, и по RFC 9000 инициировать его может **только клиент**. Сервер не может попросить клиента переехать; единственное исключение — механизм Preferred Address, который в quic-go на момент написания не реализован. Поэтому в примере ниже вся логика миграции живёт в клиенте, а серверу не нужно ни строчки: он лишь замечает, что `RemoteAddr()` изменился.

---

## 3. Сводная таблица: TCP vs QUIC

| Характеристика | TCP + TLS 1.3 | QUIC (HTTP/3) |
| :--- | :--- | :--- |
| **Базовый транспорт** | IP (протокол 6) | UDP (протокол 17) |
| **Где живёт стек** | Ядро ОС | Приложение (user space) |
| **Задержка рукопожатия** | 2 RTT (1 RTT при TLS resumption) | **1 RTT** (0 RTT при resumption) |
| **HoL Blocking между логическими каналами** | ❌ Есть (на уровне TCP) | **✓ Нет** (изоляция на уровне стримов) |
| **Идентификатор соединения** | 4-tuple (`IP:Port <-> IP:Port`) | **Connection ID** (набор CID на каждую сторону) |
| **Смена сети** | ❌ Разрыв соединения | **✓ Connection Migration** |
| **Шифрование заголовков** | ❌ Открыты (Seq, ACK, Window) | **✓ Зашифровано почти всё**, кроме флагов, версии и CID |
| **Управление перегрузкой** | Алгоритм задаёт ядро (по умолчанию Cubic; BBR — если включён администратором) | Выбирает приложение; в quic-go реализован Cubic |

---

## 4. Практика: Connection Migration на Go

### Что понадобится

* **Go.** Примеры собраны на Go 1.26; минимально нужен 1.24 из-за `slog.DiscardHandler`.
* **Библиотека** [quic-go](https://github.com/quic-go/quic-go) — самая распространённая реализация QUIC на Go: `go get github.com/quic-go/quic-go`.
* **TLS обязателен.** QUIC не бывает без шифрования, поэтому сертификат нужен даже локально. Сгенерируем самоподписанный прямо в памяти.
* **Открытый UDP.** Локальный файрвол не должен резать выбранные порты.

> ⚠️ Публичный API quic-go заметно менялся от версии к версии, и в сети много устаревших примеров. Всё ниже проверено на **v0.61.0** и **v0.63.0**. Главные отличия от старых примеров: тип соединения — `*quic.Conn` (не интерфейс `quic.Connection`), а миграция делается методом `(*quic.Conn).AddPath`, а не выдуманным `conn.Control()`.

### Структура проекта

```text
quic-demo/
├── cmd/
│   ├── migration/main.go    # демо Connection Migration
│   ├── multistream/main.go  # демо независимости стримов (QUIC и TCP)
│   └── http3/main.go        # HTTP/3 + HTTP/2 fallback
└── internal/
    ├── alpn/                # общий идентификатор ALPN
    ├── tlsconfig/           # генерация TLS-сертификата в памяти
    ├── server/              # QUIC-сервер
    ├── client/              # QUIC-клиент с поддержкой миграции
    ├── web/                 # HTTP/3 и HTTP/2 поверх одного хендлера
    ├── gapstats/            # статистика интервалов между сообщениями
    └── qlogging/            # включение qlog по переменной QLOGDIR
```

### TLS-конфигурация (`internal/tlsconfig`)

QUIC требователен к TLS: протокол **обязан** использовать TLS 1.3 и согласовать **ALPN** (Application-Layer Protocol Negotiation). С пустым `NextProtos` библиотека откажется работать. Генерируем самоподписанный сертификат на лету:

```go
// NewCertificate генерирует самоподписанный ECDSA-сертификат для localhost.
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
	// ... x509.CreateCertificate + tls.X509KeyPair
}

// ServerWithCert собирает серверный tls.Config поверх готового сертификата.
func ServerWithCert(cert tls.Certificate, alpns ...string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		NextProtos:   alpns,
	}
}

// NewServer возвращает серверный tls.Config со свежим сертификатом.
func NewServer(alpns ...string) (*tls.Config, error) {
	cert, err := NewCertificate()
	if err != nil {
		return nil, err
	}
	return ServerWithCert(cert, alpns...), nil
}

// NewInsecureClient — только для локальных демо и тестов.
func NewInsecureClient(alpns ...string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         alpns,
	}
}
```

### Сервер (`internal/server`)

Сервер принимает соединения, разводит стримы по горутинам и отдаёт данные в колбэк. Обратите внимание на структуру `Message`: вместе с данными передаётся `StreamID` (пригодится в разделе 5) и **текущий** `RemoteAddr` — при миграции он изменится сам, без каких-либо действий со стороны сервера.

```go
// Message — прикладная порция данных, прочитанная из стрима.
type Message struct {
	// Remote — текущий адрес клиента. После миграции пути он меняется сам собой.
	Remote   net.Addr
	StreamID quic.StreamID
	Data     []byte
}

type MessageHandler func(Message)

func Listen(cfg Config) (*Server, error) {
	if cfg.TLSConfig == nil {
		return nil, errors.New("tls config is required")
	}
	if len(cfg.TLSConfig.NextProtos) == 0 {
		return nil, errors.New("tls config NextProtos (ALPN) is required")
	}

	ln, err := quic.ListenAddr(cfg.Addr, cfg.TLSConfig, cfg.QUICConfig)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	// ... логгер по умолчанию, если не задан
	return &Server{listener: ln, onMessage: cfg.OnMessage, log: log}, nil
}

func (s *Server) Serve(ctx context.Context) error {
	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, quic.ErrServerClosed) {
				return nil
			}
			// ... обработка отмены контекста
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn *quic.Conn) {
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
```

Ключевой момент: **в серверном коде нет ни слова про миграцию**. Это прямое следствие того, что по RFC 9000 активная миграция — прерогатива клиента.

### Клиент с поддержкой Connection Migration (`internal/client`)

Здесь и живёт вся суть. Для миграции недостаточно «голого» `quic.DialAddr`: нужен контроль над UDP-сокетами, а значит — явный `quic.Transport`. Каждый путь (Wi-Fi, LTE) — это свой `Transport` поверх своего сокета.

Миграция делается в три шага:

```go
// Conn — обёртка над соединением и его основным стримом.
type Conn struct {
	mu     sync.Mutex
	conn   *quic.Conn
	stream *quic.Stream
}

// Migrate проверяет новый сетевой путь на next и переключает соединение на него.
// Вызывающий сохраняет владение next и должен закрыть его после Conn.Close.
func (c *Conn) Migrate(ctx context.Context, next *quic.Transport) error {
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

	// Если у контекста нет дедлайна, ограничиваем валидацию тремя секундами.
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
	// Switch ставит переключение в очередь, применяет его event loop соединения
	// на следующей итерации.
	if err := path.Switch(); err != nil {
		_ = path.Close()
		return fmt.Errorf("switch path: %w", err)
	}
	return nil
}
```

Три момента, которые в документации не подсвечены:

1. **`Probe` обязателен.** Если вызвать `Switch()` сразу после `AddPath()`, получите `quic.ErrPathNotValidated`. Сначала путь должен пройти `PATH_CHALLENGE`/`PATH_RESPONSE`.
2. **`Switch()` асинхронен.** Он лишь ставит переключение в очередь, а применяет его event loop соединения на следующей итерации. Два следствия. Первое: пакет, который event loop уже собирал в момент вызова, может уйти ещё по старому пути — данные не потеряются, но гарантии «первое же сообщение после `Switch()` придёт с нового адреса» нет (в тесте из репозитория это учтено: сообщения шлются, пока сервер не увидит новый адрес). Второе: `conn.LocalAddr()` какое-то время продолжит возвращать старый адрес, и демо будет выглядеть сломанным, хотя работает правильно.
3. **Не закрывайте старый транспорт сразу.** Владение сокетами остаётся у вызывающего кода; преждевременный `Close` старого `Transport` может уронить соединение.

#### Почему нельзя просто «подождать, пока LocalAddr обновится»

Напрашивается очевидное решение: покрутить цикл, пока `conn.LocalAddr()` не совпадёт с адресом нового сокета. Ровно так было сделано в первой версии этого кода — и `go test -race` показал, почему так делать не стоит:

```text
WARNING: DATA RACE
Write by goroutine 17:
  quic-go.(*Conn).switchToNewPath()
  quic-go.(*Conn).run()

Previous read by goroutine 9:
  quic-go.(*Conn).LocalAddr()
  internal/client.(*Conn).Migrate()
```

При переключении пути quic-go подменяет внутренний `sendConn`:

```go
func (c *Conn) switchToNewPath(tr *Transport, now monotime.Time) {
	// ...
	c.conn = newSendConn(tr.conn, c.conn.RemoteAddr(), packetInfo{}, utils.DefaultLogger) // TODO: find a better way
```

а `LocalAddr()` читает то же поле без синхронизации:

```go
func (c *Conn) LocalAddr() net.Addr { return c.conn.LocalAddr() }
```

Пометка `TODO: find a better way` намекает, что авторы про это место знают. Практический вывод: **не опрашивайте `LocalAddr()` во время миграции**. Актуальный адрес нового пути и так известен — это адрес сокета, который вы сами создали: `tr2.Conn.LocalAddr()`.

### Точка входа (`cmd/migration/main.go`)

Клиент и сервер живут в одном процессе на loopback — это осознанное упрощение, чтобы демо запускалось одной командой без второй машины. Второй путь привязан к `127.0.0.2`: на Linux вся сеть `127.0.0.0/8` висит на `lo`, поэтому у клиента меняется не только порт, но и IP. На macOS этот адрес нужно добавить руками: `sudo ifconfig lo0 alias 127.0.0.2 up`.

```go
// Два «интерфейса» клиента: условный Wi-Fi и условный LTE.
var (
	wifiAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5001}
	lteAddr  = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 7007}
)

func runDemo(ctx context.Context, log *slog.Logger, serverAddr string) error {
	tlsClient := tlsconfig.NewInsecureClient(alpn.Protocol)

	tr1, err := newTransport(wifiAddr)
	if err != nil {
		return err
	}
	defer tr1.Close()

	log.Info("connecting", "via", wifiAddr, "as", "Wi-Fi")
	conn, err := client.Dial(ctx, tr1, serverAddr, tlsClient, qlogging.Config())
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.Send([]byte("hello from Wi-Fi")); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	time.Sleep(500 * time.Millisecond)

	tr2, err := newTransport(lteAddr)
	if err != nil {
		return err
	}
	defer tr2.Close()

	log.Info("migrating", "to", lteAddr, "as", "LTE")
	migrateCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := conn.Migrate(migrateCtx, tr2); err != nil {
		return err
	}
	// Адрес берём у транспорта: conn.LocalAddr() какое-то время ещё будет отдавать старый путь.
	log.Info("migrated", "local", tr2.Conn.LocalAddr())

	// Switch применяется event loop'ом асинхронно; даём ему итерацию,
	// чтобы следующее сообщение наверняка ушло уже по новому пути.
	time.Sleep(100 * time.Millisecond)

	// Тот же стрим — без нового соединения и повторного TLS-рукопожатия.
	if err := conn.Send([]byte("hello from LTE; connection kept")); err != nil {
		return fmt.Errorf("send after migrate: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

func newTransport(addr *net.UDPAddr) (*quic.Transport, error) {
	udpConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %s: %w", addr, err)
	}
	return &quic.Transport{Conn: udpConn}, nil
}
```

### Результат выполнения

Сервер и клиент пишут в один `slog`-логгер (метка времени убрана, чтобы не шуметь):

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

Сервер получил второе сообщение из **того же стрима**, но `remote` изменился с `127.0.0.1:5001` на `127.0.0.2:7007`. Соединение не пересоздавалось, TLS-рукопожатие повторно не проводилось.

> При первом запуске вы почти наверняка увидите в логах `failed to sufficiently increase receive buffer size`. Это не ошибка вашего кода: quic-go пытается расширить UDP-буферы ядра и упирается в системный лимит (подробности — в разделе 7). Для локального демо предупреждение гасится переменной `QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING=true`.

### Что показывает qlog

Логи приложения — слабое доказательство: мало ли что там напечатал логгер. Посмотрим на реальные фреймы.

quic-go умеет писать структурированный лог событий в формате qlog; файлы `.sqlog` открываются в веб-визуализаторе [qvis](https://qvis.quictools.info/). Включается парой строк:

```go
// Config возвращает quic.Config с трассировкой в qlog, если задан QLOGDIR.
func Config() *quic.Config {
	if os.Getenv("QLOGDIR") == "" {
		return nil
	}
	return &quic.Config{Tracer: qlog.DefaultConnectionTracer}
}
```

Запускаем демо с указанием каталога и первым делом считаем фреймы валидации пути в клиентском логе:

```console
$ QLOGDIR=/tmp/qlogs go run ./cmd/migration
$ grep -o 'path_challenge\|path_response' /tmp/qlogs/*_client.sqlog | sort | uniq -c
      2 path_challenge
      2 path_response
```

Почему по два, если клиент один раз спросил, а сервер один раз ответил? Потому что валидация двусторонняя, как мы разбирали в разделе 2: один `PATH_CHALLENGE` клиент отправил, второй — получил от сервера; один `PATH_RESPONSE` получил, второй — отправил в ответ на встречный вызов.

Теперь самое интересное — с какими Connection ID уходят пакеты. Формат `.sqlog` — это JSON Text Sequences (RFC 7464), `jq` читает его с флагом `--seq`. Выберем пакеты с фреймами `stream`, `path_challenge` и `path_response` и напечатаем для каждого Destination CID:

```console
$ jq -r --seq 'select((.name // "") | test("packet_(sent|received)"))
    | select([.data.frames[]?.frame_type] | any(IN("path_challenge","path_response","stream")))
    | "\(.time|floor)ms\t\(.name|sub("transport:";""))\t\(.data.header.dcid)\t\([.data.frames[].frame_type]|join(","))"' \
    /tmp/qlogs/*_client.sqlog
1ms     packet_sent      bb20a177   stream                          # hello from Wi-Fi, старый путь
502ms   packet_sent      da611e7b   path_challenge                  # проба нового пути — новый CID
502ms   packet_received  1f2311ca   path_challenge,path_response    # ответ сервера + встречный вызов
502ms   packet_sent      bb20a177   ack,path_response               # ответ на встречный вызов
502ms   packet_sent      bb20a177   ack,stream                      # hello from LTE — снова старый CID
```

(Комментарии после `#` добавлены вручную; сами CID в вашем запуске будут другими.)

Что здесь видно:

* На этапе рукопожатия сервер выдал клиенту три запасных CID фреймами `NEW_CONNECTION_ID` (в этом логе их можно найти тем же `grep`), клиент серверу — ещё три.
* Проба нового пути ушла с **новым** DCID `da611e7b` — ровно как требует RFC 9000 §9.5. Сервер тоже поменял CID для своего встречного вызова: пакет пришёл с DCID `1f2311ca`, а не с тем, что использовался до этого.
* А вот данные после `Switch()` — сообщение `hello from LTE` — ушли с нового адреса `127.0.0.2:7007`, но со **старым** DCID `bb20a177`, тем же, что использовался с `127.0.0.1:5001`.

Последний пункт — отступление от RFC 9000 §9.5: «An endpoint MUST NOT reuse a connection ID when sending from more than one local address». На работоспособность это не влияет — сервер узнаёт соединение по любому выданному CID, и сообщение доставлено. Речь о приватности: пассивный наблюдатель, видящий оба пути, по общему CID связывает старый и новый адрес одного клиента, что раздел 9.5 и призван предотвратить. В коде quic-go причина видна: `switchToNewPath` подменяет сокет и сбрасывает congestion control, но не трогает менеджер Connection ID. Это известная проблема — issue [#5236](https://github.com/quic-go/quic-go/issues/5236) открыт с июня 2025 года, а PR [#5819](https://github.com/quic-go/quic-go/pull/5819) с ротацией CID при переключении пути на момент написания не влит. Поведение воспроизводится и на v0.61.0, и на свежей v0.63.0.

Мораль для практики: если приватность миграции для вас критична (а для мобильного клиента это может быть так), стоит проверять поведение конкретной версии библиотеки по qlog, а не по документации.

### Автотест

В репозитории миграция зафиксирована тестом, поэтому её нельзя «сломать молча». Проверяем не внутреннее состояние клиента, а наблюдаемый эффект: сервер получает данные с нового адреса в том же соединении.

```go
if err := conn.Migrate(ctx, tr2); err != nil {
	t.Fatalf("migrate: %v", err)
}

// Настоящее доказательство миграции — сервер видит данные с нового
// адреса, причём в том же самом соединении и стриме.
//
// Switch асинхронен: пакет, который event loop собирал в момент
// переключения, может уйти ещё по старому пути. Поэтому шлём сообщения
// до тех пор, пока сервер не увидит новый адрес.
path2 := tr2.Conn.LocalAddr().String()
waitRemote(t, ctx, remotes, path2, func() error {
	return conn.Send([]byte("on path 2"))
})
```

Первая версия теста отправляла одно сообщение и ждала его с нового адреса — и падала примерно в одном прогоне из тридцати. Так и выяснилось, что «данные после `Switch()` уходят по новому пути» — утверждение с оговоркой.

Тесты имеет смысл запускать с детектором гонок — именно он поймал проблему с `LocalAddr()`:

```bash
go test -race ./...
```

---

## 5. Практика: стримы не мешают друг другу — на чистом канале и под потерями

Отсутствие HoL-блокировки — тезис, который легко заявить и трудно показать. Сразу договоримся, что именно мы измеряем.

Сценарий: в одном соединении два канала. Первый непрерывно льёт данные и старается занять всё, что дадут; второй раз в 25 мс отправляет короткое управляющее сообщение. На стороне сервера измеряем интервалы между приходом управляющих сообщений — p50, p95 и максимум. Если каналы мешают друг другу, интервалы «расползаются».

Сценарий прогоняем в двух вариантах:

* **QUIC** — два стрима одного соединения;
* **TCP+TLS** — одно соединение, в которое оба канала упакованы кадрами `[тип][длина][данные]`. Это модель того, как HTTP/2 мультиплексирует стримы поверх одного TCP-потока.

И в двух средах: на чистом loopback и под эмулированными потерями. **На чистом loopback пакеты не теряются**, поэтому первый прогон показывает только честность планировщика стримов и per-stream flow control — а не преимущество над TCP. Разница появляется во втором прогоне.

Код клиентской части общий для обоих транспортов — меняются лишь функции отправки:

```go
// drive запускает две конкурирующие «нагрузки»: непрерывную заливку и
// короткие сообщения с фиксированным интервалом. Транспорт задаётся снаружи.
func drive(sendBulk, sendControl func([]byte) error) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Тяжёлый стрим: старается занять весь канал.
	go func() {
		defer wg.Done()
		chunk := make([]byte, bulkChunk) // 64 KiB
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
		for i := 1; i <= controlRounds; i++ { // 40 сообщений
			if err := sendControl(fmt.Appendf(nil, "ping #%02d\n", i)); err != nil {
				return
			}
			time.Sleep(controlEvery) // 25 мс
		}
	}()

	wg.Wait()
	time.Sleep(settleTime) // ждём хвост данных, уже находящихся в пути
}
```

Для QUIC `sendBulk` пишет во второй стрим, `sendControl` — в основной:

```go
drive(
	func(chunk []byte) error { _, err := bulk.Write(chunk); return err },
	conn.Send,
)
```

Для TCP обе функции пишут кадры в один сокет под общим мьютексом — как это делает любой мультиплексор поверх TCP.

### Чистый loopback

```console
$ go run ./cmd/multistream
control stream id=0, bulk stream id=4

[server] control ping #01 at     2.5 ms | bulk received:    0.00 MiB
[server] control ping #02 at    27.4 ms | bulk received:   15.01 MiB
[server] control ping #03 at    52.5 ms | bulk received:   31.97 MiB
...
[server] control ping #40 at   979.8 ms | bulk received:  672.14 MiB

transport: QUIC, two streams in one connection
bulk delivered: 689.06 MiB
control messages: 40/40, send interval: 25 ms
gap p50 / p95 / max: 25.1 / 25.3 / 25.3 ms
```

```console
$ go run ./cmd/multistream -tcp
...
transport: TCP+TLS, two channels framed into one connection
bulk delivered: 1221.62 MiB
control messages: 40/40, send interval: 25 ms
gap p50 / p95 / max: 26.1 / 26.1 / 26.2 ms
```

Без потерь оба транспорта держат расписание: джиттер — доли миллисекунды. У TCP чуть больше пропускная способность (ядро, TSO, никакого шифрования каждого пакета в user space) и на миллисекунду больше интервал — управляющий кадр ждёт, пока в сокет допишется очередной 64-килобайтный кусок заливки. Пока ничего драматичного.

### Под потерями

Эмулировать плохую сеть можно `tc netem`, но вешать его на `lo` хост-системы — плохая идея: он затронет весь локальный трафик (базы, IDE, Docker) и действует в обе стороны. Безопаснее сделать это в отдельном сетевом пространстве имён, без `sudo` и без последствий для системы:

```bash
go build -o /tmp/multistream ./cmd/multistream

# 20 мс задержки (в обе стороны, итого RTT 40 мс) и 2% потерь — только внутри namespace
unshare -Urn sh -c 'ip link set lo up &&
  tc qdisc add dev lo root netem delay 20ms loss 2% &&
  /tmp/multistream'

unshare -Urn sh -c 'ip link set lo up &&
  tc qdisc add dev lo root netem delay 20ms loss 2% &&
  /tmp/multistream -tcp'
```

Namespace живёт ровно до завершения команды — ничего откатывать не нужно. Если всё же ставите netem на настоящий `lo`, не забудьте `sudo tc qdisc del dev lo root netem`.

Результаты (два прогона каждого варианта, разброс между прогонами небольшой):

```console
transport: QUIC, two streams in one connection
bulk delivered: 1.00 MiB
control messages: 40/40, send interval: 25 ms
gap p50 / p95 / max: 26.7 / 40.2 / 40.2 ms
```

```console
transport: TCP+TLS, two channels framed into one connection
bulk delivered: 63.88 MiB
control messages: 40/40, send interval: 25 ms
gap p50 / p95 / max: 120.6 / 333.8 / 363.6 ms
```

| Вариант | p50 | p95 | max |
| :--- | ---: | ---: | ---: |
| QUIC, loopback | 25.1 мс | 25.3 мс | 25.3 мс |
| TCP, loopback | 26.1 мс | 26.1 мс | 26.2 мс |
| QUIC, 2 % потерь + 40 мс RTT | 26.7 мс | 40.2 мс | 40.2 мс |
| TCP, 2 % потерь + 40 мс RTT | 120.6 мс | 333.8 мс | 363.6 мс |

У QUIC управляющий стрим под потерями почти не заметил соседа: p50 остался около 25 мс, а максимум — 40 мс, то есть примерно один RTT: столько стоит повторная передача, если потерялся пакет с самим пингом. У TCP медиана выросла почти в пять раз, а хвост — до трети секунды: управляющий кадр стоит в одной очереди байтов с мегабайтами заливки и ждёт, пока ядро дотащит до получателя всё, что было отправлено раньше, включая потерянные и повторно переданные сегменты. Это и есть HoL-блокировка, только теперь в цифрах. Причём на TCP-варианте под потерями весь прогон растянулся с одной секунды до почти шести: блокировалась не только доставка, но и сама отправка — `Write` управляющего кадра ждал, пока освободится место в буфере сокета за заливкой.

Две оговорки, чтобы эксперимент нельзя было упрекнуть в подтасовке:

* **Пропускную способность сравнивать нельзя.** QUIC под потерями залил 1 MiB против 64 MiB у TCP — но `netem` теряет *пакеты*, а не байты. На loopback TCP отправляет сегменты до 64 КБ (GSO/TSO), тогда как QUIC-пакет — около 1,2 КБ; при одинаковых 2 % потерь на байт QUIC теряет в десятки раз чаще. Плюс congestion control quic-go (Cubic) на случайных потерях ведёт себя консервативно. Метрика этого эксперимента — интервалы между управляющими сообщениями, а не мегабайты.
* **TCP-вариант — модель, а не HTTP/2.** Реальный HTTP/2 приоритизирует стримы и режет кадры мельче, что смягчит картину, но не изменит её сути: всё уходит в один упорядоченный поток байтов, и потерянный сегмент держит за собой всё, что было после него.

---

## 6. Практика: HTTP/3 и fallback на HTTP/2

QUIC в чистом виде нужен нечасто — гораздо чаще его используют как транспорт для HTTP/3. И почти всегда рядом должен стоять fallback: как увидим в разделе 7, UDP далеко не везде проходит.

Хорошая новость: `http3.Server` из состава quic-go принимает обычный `http.Handler`, поэтому один и тот же хендлер можно отдать сразу по UDP (HTTP/3) и по TCP (HTTP/2 + HTTP/1.1) на одном и том же номере порта.

```go
// Handler возвращает мультиплексор, который сообщает, по какой версии
// протокола пришёл запрос.
func Handler(h3 *http3.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if h3 != nil {
			// Alt-Svc сообщает клиенту, что тот же ресурс доступен по HTTP/3.
			_ = h3.SetQUICHeaders(w.Header())
		}
		fmt.Fprintf(w, "Hello from %s! path=%s\n", r.Proto, r.URL.Path)
	})
	return mux
}

// New собирает пару серверов с общим сертификатом.
//
// Allow0RTT здесь работает: http3.Server внутри поднимает листенер через
// quic.ListenEarly, а не quic.Listen.
func New(cert tls.Certificate, addr string) *Servers {
	h3 := &http3.Server{
		Addr:       addr,
		TLSConfig:  &tls.Config{Certificates: []tls.Certificate{cert}},
		QUICConfig: &quic.Config{Allow0RTT: true},
	}
	handler := Handler(h3)
	h3.Handler = handler

	// TLS 1.3 — как и в QUIC, чтобы сравнение транспортов было честным.
	tcp := &http.Server{
		Addr:    addr,
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h2", "http/1.1"},
		},
	}
	return &Servers{H3: h3, TCP: tcp}
}
```

ALPN для HTTP/3 (`h3`) выставляет сам `http3.Server` через `ConfigureTLSConfig` — вручную его прописывать не нужно, а вот для TCP-сервера `h2` и `http/1.1` указать обязательно. Сертификат один на оба транспорта.

Проверяем внешним клиентом, а не только своим же Go-кодом:

```console
$ go run ./cmd/http3 &
$ curl -s --http2 -k https://127.0.0.1:4433/
Hello from HTTP/2.0! path=/

$ curl -s --http1.1 -k https://127.0.0.1:4433/
Hello from HTTP/1.1! path=/

$ curl -sI --http2 -k https://127.0.0.1:4433/ | grep -i alt-svc
alt-svc: h3=":4433"; ma=2592000
```

Заголовок `Alt-Svc` — механизм апгрейда: браузер сходит по TCP, увидит анонс `h3=":4433"` и следующие запросы попробует уже по QUIC. С одной оговоркой: с самоподписанным сертификатом браузер по `Alt-Svc` не пойдёт. Chrome и Firefox не устанавливают HTTP/3-соединение, если сертификат не проходит проверку доверия, — ошибку сертификата можно «прокликать» только для TCP. Чтобы увидеть HTTP/3 в браузере, нужен сертификат от доверенного CA (для локальной разработки подойдёт `mkcert`).

С проверкой HTTP/3 через curl тоже есть нюанс: системный curl обычно собран **без** поддержки HTTP/3:

```bash
curl --version | grep -o HTTP3
```

Если вывод пуст, `--http3-only` не заработает. Нужна сборка curl с HTTP/3-бэкендом — ngtcp2 или quiche (на macOS её ставит `brew install curl`, на Linux проще всего взять статическую сборку с сайта curl или собрать самому). Чтобы не зависеть от окружения, в репозитории HTTP/3 проверяется Go-тестом через `http3.Transport`:

```go
tr := &http3.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
}
defer tr.Close()

resp, err := (&http.Client{Transport: tr}).Get(url)
// resp.Proto == "HTTP/3.0"
```

Проверка `resp.Proto` — самый быстрый способ убедиться, что запрос действительно ушёл по QUIC, а не по TCP.

> Ещё одно отличие от старых примеров: клиент там строится на `http3.RoundTripper`. В актуальных версиях quic-go этот тип называется `http3.Transport`.

---

## 7. Обратная сторона медали: QUIC в production

QUIC выглядит привлекательно, но у него есть эксплуатационные особенности, с которыми сталкиваются команды в проде.

### Нагрузка на CPU

Весь протокол живёт в user space: каждый UDP-пакет проходит через системный вызов, шифруется и разбирается приложением. На высоких скоростях — десятки Гбит/с, миллионы пакетов в секунду на сервер — TCP выигрывает за счёт аппаратного offloading в сетевых картах (TSO/LRO) и оптимизированного стека ядра. Точная цифра «с какого RPS становится больно» зависит от размера ответов, ядра и железа, поэтому мерить её нужно на своём профиле нагрузки.

* **Что помогает:** UDP GSO/GRO в Linux (quic-go включает GSO по умолчанию, где оно поддерживается), а также eBPF/XDP для фильтрации мусора до попадания в user space.
* **Платформенная разница:** на Linux набор оптимизаций самый полный; на macOS часть недоступна, поэтому бенчмарки имеет смысл снимать на целевой ОС, а не на ноутбуке разработчика.

### UDP-буферы ядра

При интенсивном трафике стандартный UDP-буфер Linux (около 200 КБ) быстро переполняется, и пакеты теряются ещё до QUIC-стека. Поэтому quic-go при старте предупреждает, если не смог поднять буфер до желаемых ~7 MiB.

* **Решение:** поднять лимиты ядра:
  ```bash
  sysctl -w net.core.rmem_max=25000000
  sysctl -w net.core.wmem_max=25000000
  ```
* Для локальной разработки предупреждение гасится `QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING=true`, но в проде лимиты лучше всё-таки поднять. Подробности — в [wiki quic-go](https://github.com/quic-go/quic-go/wiki/UDP-Buffer-Sizes).

### Блокировка UDP файрволами и middleboxes

Часть корпоративных сетей, провайдеров и публичных Wi-Fi режет исходящий UDP кроме DNS. Причина не столько в amplification-атаках (это проблема серверов-рефлекторов, а не клиентов), сколько в неудобстве UDP для stateful-инспекции: нет соединения, которое можно отслеживать, а содержимое QUIC зашифровано и для DPI непрозрачно.

* **Решение:** клиенты всегда реализуют **fallback**. Браузеры пробуют QUIC и TCP параллельно (или TCP с небольшой задержкой) и используют то, что ответило первым; конкретные тайминги зависят от реализации и накопленной статистики про сеть. Рабочий пример двухтранспортной схемы на стороне сервера — в разделе 6.

### Ограничения и цена миграции

* Активную миграцию инициирует только клиент; сервер не может её запросить (Preferred Address в quic-go пока не поддержан).
* Сервер вправе запретить миграцию транспортным параметром `disable_active_migration` — тогда `AddPath` вернёт ошибку.
* Путь обязательно проходит валидацию, то есть переключение стоит как минимум один RTT.
* Переключение не бесплатно и для пропускной способности. В quic-go при смене пути вызывается `MigratedPath`: статистика RTT сбрасывается, congestion window возвращается к начальному значению, а все пакеты, находившиеся в полёте по старому пути, объявляются потерянными и переотправляются. Для тяжёлой загрузки это означает заметный провал скорости сразу после миграции — тот же slow start, что и у нового соединения, только без рукопожатия.

---

## 8. Заключение

QUIC — не «TCP поверх UDP», а транспорт, спроектированный под нестабильные сети, меняющиеся адреса и жёсткие требования к задержкам. В статье мы проверили три его свойства на реальном трафике:

* **Миграция работает**, стрим переживает смену IP и порта, и qlog это подтверждает. Он же показал, что quic-go пока не меняет CID при переключении пути, как требует RFC 9000 §9.5 — библиотеку стоит проверять, а не только читать про неё.
* **Стримы независимы**: под 2 % потерь управляющий канал QUIC отклонился от расписания максимум на один RTT, тогда как в одном TCP-соединении медиана интервалов выросла впятеро.
* **HTTP/3 и HTTP/2 живут на одном хендлере**, и fallback на TCP собирается в несколько строк.

**Где QUIC оправдан:**

* **Мобильные клиенты** — сглаживает переключение между Wi-Fi и сотовой сетью.
* **Стриминг и real-time медиа** — управляющие команды и тяжёлый видеопоток в разных стримах не блокируют друг друга, как в эксперименте из раздела 5.
* **Межсервисное взаимодействие через ненадёжные сети** — например, между регионами или до edge-узлов: потери там реальны, а HoL-блокировка одного мультиплексированного соединения бьёт по всем запросам сразу. Внутри одного датацентра с persistent-соединениями HTTP/2 выигрыш от QUIC невелик, а CPU он ест больше.
* **Долгоживущие соединения с несколькими логическими каналами** — агенты, телеметрия, управление устройствами. С оговоркой: на слабом IoT-железе user-space-стек с шифрованием каждого пакета может оказаться дороже TCP, и это нужно измерять.

Не стоит ждать чуда там, где узкое место — диск, база данных или где UDP просто недоступен. Но как транспорт для долгоживущего мультиплекса, переживающего смену сети, QUIC уже вполне взрослый инструмент, а quic-go даёт к нему прямой доступ из Go.

### Что почитать дальше

* [RFC 9000](https://www.rfc-editor.org/rfc/rfc9000.html) — QUIC: транспортный протокол; раздел 9 — про миграцию и Connection ID.
* [RFC 9001](https://www.rfc-editor.org/rfc/rfc9001.html) — использование TLS 1.3 в QUIC, включая header protection.
* [RFC 9002](https://www.rfc-editor.org/rfc/rfc9002.html) — детекция потерь и congestion control.
* [RFC 9114](https://www.rfc-editor.org/rfc/rfc9114.html) — HTTP/3.
* [RFC 9221](https://www.rfc-editor.org/rfc/rfc9221.html) — ненадёжные датаграммы поверх QUIC.
* [RFC 9369](https://www.rfc-editor.org/rfc/rfc9369.html) — QUIC версии 2: те же механизмы, другие константы, чтобы сеть не «окостенела» вокруг v1.
* [Документация quic-go](https://quic-go.net/docs/) — в частности раздел про [Connection Migration](https://quic-go.net/docs/quic/connection-migration/).
* [quic-go#5236](https://github.com/quic-go/quic-go/issues/5236) — обсуждение ротации Connection ID при переключении пути.
