# dns-box

[![Go Version](https://img.shields.io/badge/go-1.25.3-blue)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Высокопроизводительный DNS-сервер на Go с поддержкой маршрутизации через VPN, блокировки рекламы и автоматического резервного копирования конфигурации в GitHub.

## Оглавление

- [Возможности](#возможности)
- [Архитектура](#архитектура)
- [Установка](#установка)
- [Конфигурация](#конфигурация)
- [Запуск](#запуск)
- [HTTP API](#http-api)
- [Интеграция с ipset](#интеграция-с-ipset)
- [Блокировка рекламы и трекеров](#блокировка-рекламы-и-трекеров)
- [Резервное копирование в GitHub](#резервное-копирование-в-github)
- [Кеширование](#кеширование)
- [Сборка и деплой](#сборка-и-деплой)
- [Тестирование](#тестирование)
- [Структура проекта](#структура-проекта)

---

## Возможности

- **DNS-резолвинг** с поддержкой нескольких upstream-серверов
- **Протоколы**: UDP, TCP, DNS-over-HTTPS (DoH), DNS-over-TLS (DoT)
- **Маршрутизация через VPN** - автоматическое добавление IP-адресов указанных доменов в Linux ipset
- **Блокировка рекламы и трекеров** - загрузка внешних блоклистов (формат hosts)
- **Кеширование DNS-запросов** с настраиваемым TTL
- **HTTP API** для управления доменами, суффиксами и блоклистами
- **Резервное копирование конфигурации в GitHub** - ваши правила не потеряются
- **Автоматическое восстановление** - при пустом локальном конфиге домены загружаются из GitHub
- **Приоритизация upstream-серверов** - DoH/DoT используются в первую очередь, plain DNS как fallback

---

## Архитектура

```
                    ┌─────────────────────────────────────────┐
                    │           dns-box (порт 953)            │
                    │                                         │
  Клиент ──UDP──►   │  ┌───────────┐    ┌──────────────────┐  │
  DNS-запрос        │  │  Handler   │───►│   DNS Resolver   │──┼──► Upstream DNS
                    │  │            │    │  (DoH/DoT/UDP)   │  │
                    │  └─────┬─────┘    └──────────────────┘  │
                    │        │                                 │
                    │        ▼                                 │
                    │  ┌───────────┐    ┌──────────────────┐  │
                    │  │  ipset    │    │   BlockList      │  │
                    │  │  (VPN)    │    │  (блокировка)    │  │
                    │  └───────────┘    └──────────────────┘  │
                    │        ▲                                 │
                    │        │                                 │
                    │  ┌─────┴─────┐    ┌──────────────────┐  │
                    │  │  Cache    │    │   DomainCache    │  │
                    │  │  (DNS)    │    │  (правила)       │  │
                    │  └───────────┘    └──────────────────┘  │
                    └─────────────────────────────────────────┘
                                         │
                    ┌────────────────────┴────────────────────┐
                    │         HTTP API (порт 8090)            │
                    │  /domains  /suffixes  /blocklist/urls   │
                    └─────────────────────────────────────────┘
                                         │
                    ┌────────────────────┴────────────────────┐
                    │         GitHub Backup (опционально)     │
                    │  hosts_config.json в вашем репозитории  │
                    └─────────────────────────────────────────┘
```

---

## Установка

### Требования

- Go 1.25.3 или выше
- Linux (для работы ipset)
- Права root или CAP_NET_ADMIN для работы с ipset

### Клонирование

```bash
git clone https://github.com/your-username/dns-box.git
cd dns-box
```

### Сборка

```bash
# Локальная сборка
go build -o dns-box ./cmd/dns-box/main.go

# Для ARM (роутеры, Raspberry Pi)
make arm-build
```

---

## Конфигурация

Конфигурационный файл `config.json`:

```json
{
  "server": {
    "address": ["127.0.0.1:953", "[::]:953"],
    "log": "debug"
  },
  "dns": {
    "upstream_servers": [
      "https://dns.google/dns-query",
      "tls://1.1.1.1",
      "8.8.8.8:53"
    ],
    "cache_ttl": 3600
  },
  "ipset": {
    "lists": [
      {
        "name": "vpn_domains",
        "enable_ipv6": true,
        "timeout": 7200,
        "rules": {
          "domain": [
            "rutor.is",
            "rutracker.org"
          ],
          "domain_suffix": [
            ".googlevideo.com",
            ".youtube.com"
          ]
        }
      }
    ]
  },
  "blocklist": {
    "enabled": true,
    "urls": [
      "https://blocklistproject.github.io/Lists/tracking.txt"
    ],
    "refresh_hours": 24
  },
  "github_backup": {
    "enabled": true,
    "token": "ghp_your_personal_access_token",
    "owner": "your-username",
    "repo": "dns-box-config",
    "path": "hosts_config.json",
    "branch": "main"
  }
}
```

### Описание секций

#### `server`

| Параметр | Тип | Описание |
|----------|-----|----------|
| `address` | `[]string` | Список адресов для прослушивания (поддержка IPv4 и IPv6) |
| `log` | `string` | Уровень логирования: `debug`, `info`, `warn`, `error`, `trace` |

#### `dns`

| Параметр | Тип | Описание |
|----------|-----|----------|
| `upstream_servers` | `[]string` | Список upstream DNS-серверов с приоритетом |
| `cache_ttl` | `int` | Время жизни кеша в секундах (по умолчанию) |

**Поддерживаемые протоколы upstream:**

| Префикс | Протокол | Порт по умолчанию |
|---------|----------|-------------------|
| `https://` или `doh://` | DNS-over-HTTPS | 443 |
| `tls://` или `dot://` | DNS-over-TLS | 853 |
| `tcp://` | TCP DNS | 53 |
| `udp://` или без префикса | UDP DNS | 53 |

**Приоритет использования:** DoH → DoT → TCP → UDP

#### `ipset` - настройка маршрутизации через VPN

Секция `ipset` определяет, IP-адреса каких доменов добавляются в Linux ipset для последующей маршрутизации через VPN.

**Основная идея:** dns-box слушает DNS-запросы, и когда домен совпадает с одним из правил в `rules`, IP-адрес ответа автоматически добавляется в соответствующий ipset. iptables затем направляет трафик на эти IP через VPN-туннель.

**Конфигурация через `ipset.lists`:**

Каждый элемент массива `lists` - это отдельный ipset-список с собственными правилами доменов.

| Параметр списка | Тип | Описание |
|-----------------|-----|----------|
| `name` | `string` | Базовое имя ipset. Для IPv4 используется как есть, для IPv6 автоматически добавляется суффикс `6` (например, `vpn_domains` → `vpn_domains6`) |
| `enable_ipv6` | `bool` | Создать ли дополнительный IPv6 ipset. Если `true`, создаётся второй ipset с именем `name` + `6` |
| `timeout` | `uint32` | Таймаут записей в секундах. `0` = значение по умолчанию (7200 сек = 2 часа) |
| `rules` | `RulesConfig` | Правила доменов для этого списка (см. ниже) |

**Параметры `rules` (для каждого списка):**

| Параметр | Тип | Описание |
|----------|-----|----------|
| `domain` | `[]string` | Точные домены. Совпадение только с указанным доменом |
| `domain_suffix` | `[]string` | Суффиксы доменов (начинаются с `.`). Совпадение с доменом и всеми его поддоменами |

**Примеры работы правил:**

| Правило | Совпадёт | Не совпадёт |
|---------|----------|-------------|
| `"youtube.com"` | `youtube.com` | `www.youtube.com`, `myyoutube.com` |
| `".youtube.com"` | `www.youtube.com`, `m.youtube.com` | `youtube.com`, `myyoutube.com` |

**Пример с несколькими списками:**

```json
"ipset": {
  "lists": [
    {
      "name": "vpn_domains",
      "enable_ipv6": true,
      "timeout": 7200,
      "rules": {
        "domain": ["rutracker.org", "rutor.is"],
        "domain_suffix": [".youtube.com"]
      }
    },
    {
      "name": "proxy_domains",
      "enable_ipv6": false,
      "timeout": 3600,
      "rules": {
        "domain": ["example.com"],
        "domain_suffix": [".example.org"]
      }
    }
  ]
}
```

В этом примере создаются:
- `vpn_domains` (IPv4) и `vpn_domains6` (IPv6) - для доменов из первого списка
- `proxy_domains` (только IPv4) - для доменов из второго списка

> **Обратная совместимость:** старые поля `ipv4name` и `ipv6name` продолжают работать. При их использовании правила берутся из корневой секции `rules`.

> **Важно:** ipset работает только на Linux. Таймаут записей в ipset проходит через нормализацию TTL (см. [Кеширование](#кеширование)).

#### `blocklist`

| Параметр | Тип | Описание |
|----------|-----|----------|
| `enabled` | `bool` | Включить/выключить блокировку |
| `urls` | `[]string` | URL блоклистов (HTTP/HTTPS или локальный файл) |
| `refresh_hours` | `int` | Интервал обновления блоклистов в часах |

**Формат блоклиста:** стандартный hosts-формат:
```
0.0.0.0 tracker.example.com
0.0.0.0 ads.example.com
```

#### `github_backup`

| Параметр | Тип | Описание |
|----------|-----|----------|
| `enabled` | `bool` | Включить резервное копирование в GitHub |
| `token` | `string` | GitHub Personal Access Token (с правами `repo`) |
| `owner` | `string` | Имя пользователя или организации |
| `repo` | `string` | Название репозитория |
| `path` | `string` | Путь к файлу конфигурации в репозитории |
| `branch` | `string` | Ветка репозитория |

> **Получение токена:** Settings → Developer settings → Personal access tokens → Generate new token → выбрать scope `repo`.

---

## Запуск

### Обычный запуск

```bash
./dns-box -config config.json
```

### Как сервис (systemd)

Создайте файл `/etc/systemd/system/dns-box.service`:

```ini
[Unit]
Description=DNS Box - DNS Server with VPN routing
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/dns-box -config /etc/dns-box/config.json
Restart=on-failure
RestartSec=5
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable dns-box
sudo systemctl start dns-box
sudo systemctl status dns-box
```

---

## HTTP API

API доступен на порту `8090`.

### Управление доменами

#### Получить все домены

```bash
curl http://localhost:8090/domains
```

**Ответ:**
```json
["example.com", "rutracker.org"]
```

#### Добавить домены

```bash
curl -X POST http://localhost:8090/domains \
  -d "example.com
another.com
third.org"
```

**Ответ:** `ok` или `domain example.com exist` (если уже существует)

#### Удалить домены

```bash
curl -X DELETE http://localhost:8090/domains \
  -d "example.com
another.com"
```

**Ответ:** `ok` или `domain example.com not found`

---

### Управление суффиксами

#### Получить все суффиксы

```bash
curl http://localhost:8090/suffixes
```

**Ответ:**
```json
[".youtube.com", ".googlevideo.com"]
```

#### Добавить суффиксы

```bash
curl -X POST http://localhost:8090/suffixes \
  -d ".example.com
.google.com"
```

> Суффикс автоматически дополняется точкой, если её нет: `example.com` → `.example.com`

#### Удалить суффиксы

```bash
curl -X DELETE http://localhost:8090/suffixes \
  -d ".example.com"
```

---

### Управление ipset списками

При использовании новой конфигурации `ipset.lists` каждый список имеет собственные правила доменов.

#### Получить все ipset списки

```bash
curl http://localhost:8090/ipset/lists
```

**Ответ:**
```json
[
  {
    "name": "vpn_domains",
    "enable_ipv6": true,
    "timeout": 7200,
    "rules": {
      "domain": ["rutracker.org"],
      "domain_suffix": [".youtube.com"]
    }
  },
  {
    "name": "proxy_domains",
    "enable_ipv6": false,
    "timeout": 3600,
    "rules": {
      "domain": ["example.com"],
      "domain_suffix": []
    }
  }
]
```

#### Получить домены конкретного списка

```bash
curl http://localhost:8090/ipset/vpn_domains/domains
```

**Ответ:**
```json
["rutracker.org", "example.com"]
```

#### Добавить домены в список

```bash
curl -X POST http://localhost:8090/ipset/vpn_domains/domains \
  -d "newdomain.com
another.org"
```

**Ответ:** `ok`

#### Удалить домены из списка

```bash
curl -X DELETE http://localhost:8090/ipset/vpn_domains/domains \
  -d "newdomain.com"
```

**Ответ:** `ok`

#### Получить суффиксы конкретного списка

```bash
curl http://localhost:8090/ipset/proxy_domains/suffixes
```

#### Добавить суффиксы в список

```bash
curl -X POST http://localhost:8090/ipset/vpn_domains/suffixes \
  -d ".newdomain.com
.another.org"
```

#### Удалить суффиксы из списка

```bash
curl -X DELETE http://localhost:8090/ipset/vpn_domains/suffixes \
  -d ".newdomain.com"
```

> **Примечание:** старые эндпоинты `/domains` и `/suffixes` продолжают работать для обратной совместимости с legacy конфигурацией (`ipv4name`/`ipv6name`).

---

### Управление блоклистами

#### Получить URL блоклистов

```bash
curl http://localhost:8090/blocklist/urls
```

**Ответ:**
```json
["https://blocklistproject.github.io/Lists/tracking.txt"]
```

#### Добавить URL блоклиста

```bash
curl -X POST http://localhost:8090/blocklist/urls \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/blocklist.txt"}'
```

#### Удалить URL блоклиста

```bash
curl -X DELETE http://localhost:8090/blocklist/urls \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/blocklist.txt"}'
```

---

## Интеграция с ipset

### Что такое ipset?

ipset - это механизм в Linux для хранения множеств IP-адресов, используемый совместно с iptables/nftables для эффективной маршрутизации.

### Как dns-box использует ipset

1. При старте создаются ipset списки на основе конфигурации `ipset.lists`
2. Каждый список создает IPv4 set с указанным именем и опционально IPv6 set (имя + `6`)
3. При DNS-запросе для домена из правил списка, IP-адреса ответа добавляются в соответствующий ipset
4. TTL записи в ipset проходит через нормализацию (минимум 5 мин, максимум 1 час)
5. iptables направляет трафик на эти IP через VPN

### Настройка iptables для VPN-маршрутизации

```bash
# Создать ipset (dns-box сделает это автоматически)
sudo ipset create vpn_domains hash:ip timeout 7200
sudo ipset create vpn_domains6 hash:ip family inet6 timeout 7200

# Настроить маркировку трафика
sudo iptables -t mangle -A OUTPUT -m set --match-set vpn_domains dst -j MARK --set-mark 100
sudo ip6tables -t mangle -A OUTPUT -m set --match-set vpn_domains6 dst -j MARK --set-mark 100

# Настроить policy-based routing
sudo ip rule add fwmark 100 table vpn
sudo ip route add default via 10.8.0.1 dev tun0 table vpn
```

### Пример конфигурации с несколькими ipset списками

Вы можете создать несколько ipset списков с собственными правилами для каждого:

```json
"ipset": {
  "lists": [
    {
      "name": "vpn_domains",
      "enable_ipv6": true,
      "timeout": 7200,
      "rules": {
        "domain": ["rutracker.org"],
        "domain_suffix": [".youtube.com"]
      }
    },
    {
      "name": "proxy_domains",
      "enable_ipv6": false,
      "timeout": 3600,
      "rules": {
        "domain": ["example.com"],
        "domain_suffix": [".example.org"]
      }
    }
  ]
}
```

В этом примере:
- `vpn_domains` и `vpn_domains6` - для трафика через VPN (YouTube, Rutracker)
- `proxy_domains` - только IPv4, для другого набора доменов (например, для прокси)

### Проверка содержимого ipset

```bash
# Посмотреть все записи
sudo ipset list vpn_domains
sudo ipset list vpn_domains6

# Количество записей
sudo ipset list vpn_domains | grep "Number of entries"
```

---

## Блокировка рекламы и трекеров

### Встроенные блоклисты

dns-box загружает блоклисты в формате hosts и блокирует запросы к указанным доменам, возвращая `0.0.0.0`.

### Популярные блоклисты

```json
"blocklist": {
  "urls": [
    "https://blocklistproject.github.io/Lists/tracking.txt",
    "https://blocklistproject.github.io/Lists/ads.txt",
    "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"
  ]
}
```

### Локальный блоклист

Можно использовать локальный файл:

```json
"blocklist": {
  "urls": [
    "/etc/dns-box/my-blocklist.txt"
  ]
}
```

### Проверка статуса блоклиста

Блокировка происходит автоматически при обработке DNS-запросов. Заблокированные домены логируются на уровне `debug`:

```
DEBUG Blocked domain: tracker.example.com
```

---

## Резервное копирование в GitHub

### Автоматическое сохранение

При каждом изменении доменов или суффиксов через API, конфигурация автоматически сохраняется:
1. В локальный файл `config.json`
2. В GitHub репозиторий (если `github_backup.enabled: true`)

### Формат файла в GitHub

Файл `hosts_config.json` в репозитории поддерживает **два формата**:

**Новый формат (рекомендуется)** - при использовании `ipset.lists`:

```json
{
  "ipset_lists": [
    {
      "name": "vpn_domains",
      "enable_ipv6": true,
      "timeout": 7200,
      "rules": {
        "domain": ["rutracker.org", "rutor.is"],
        "domain_suffix": [".youtube.com"]
      }
    },
    {
      "name": "proxy_domains",
      "enable_ipv6": false,
      "timeout": 3600,
      "rules": {
        "domain": ["example.com"],
        "domain_suffix": []
      }
    }
  ]
}
```

**Старый формат (legacy)** - при использовании `ipv4name`/`ipv6name`:

```json
{
  "domain": [
    "example.com",
    "rutracker.org"
  ],
  "domain_suffix": [
    ".youtube.com",
    ".googlevideo.com"
  ]
}
```

> **Что сохраняется на GitHub:**
> - При новом формате (`ipset.lists`) - сохраняются **все списки** с их правилами
> - При legacy формате - только корневые `rules`
> - Локальный `config.json` сохраняется полностью (все секции)

---

## Кеширование

### DNS-кеш

- **Библиотека:** VictoriaMetrics/fastcache
- **Размер:** 8 MB (настраивается в коде)
- **TTL:** берётся из DNS-ответа, проходит через нормализацию
- **Ключ:** `domain|query_type` (например, `google.com|1` для A-записей)

### Нормализация TTL

Все TTL из DNS-ответов проходят через политику нормализации перед использованием в кеше и ipset:

```
TTL ≤ 0     → 3600  (защита от нулевых/отрицательных значений)
TTL < 180   → 900   (короткие TTL фиксируются на 15 минут)
TTL ≥ 180   → TTL   (используется как есть)

Затем применяются жёсткие границы:
effective = max(effective, 300)   → минимум 5 минут
effective = min(effective, 3600)  → максимум 1 час
```

**Зачем это нужно:**

| Проблема | Решение |
|----------|---------|
| Некоторые домены отдают TTL = 0 | Записи не будут мгновенно протухать |
| TTL < 3 минут (например, 30 сек) | Кеш и ipset будут жить минимум 5 минут |
| TTL > 1 часа (например, 86400) | IP не будут висеть слишком долго, если домен сменил адрес |

**Примеры:**

| Исходный TTL | После нормализации |
|--------------|---------------------|
| 0 | 3600 (1 час) |
| 30 | 900 → clamp → **300** (5 мин) |
| 120 | 900 → clamp → **900** (15 мин) |
| 300 | **300** (5 мин) |
| 1800 | **1800** (30 мин) |
| 86400 | **3600** (1 час) |

Нормализация применяется к:
- ✅ Записям в **ipset** (IPv4 и IPv6)
- ✅ **DNS-кешу** (положительные ответы)
- ✅ **Negative cache** (NXDOMAIN ответы)

### Кеш доменов

- **Библиотека:** VictoriaMetrics/fastcache
- **Размер:** 8 MB
- **Содержит:** точные домены и суффиксы из `rules`
- **Использование:** быстрая проверка, нужно ли добавлять IP в ipset

### Отрицательное кеширование

DNS-ответы с `NXDOMAIN` (домен не существует) кешируются с TTL из SOA-записи, прошедшим нормализацию.

---

## Сборка и деплой

### Локальная сборка

```bash
make build
```

### Кросс-компиляция для ARM

```bash
make arm-build
```

Собирает бинарник для Linux ARM с softfloat (для роутеров и embedded-устройств).

### Сжатие бинарника (UPX)

```bash
make pack
```

Сжимает бинарник с помощью UPX (алгоритм lzma) для экономии места.

### Деплой на роутер

```bash
# Скопировать бинарник
make copyToRouter

# Установить права
make setRights

# Перезапустить сервис
make restart
```

> **Примечание:** В Makefile необходимо настроить переменные `be` (SSH-хост) и пути.

### Полный цикл

```bash
make all
```

Выполняет: `arm-build` → `pack` → `copy` → `copyToRouter` → `setRights` → `restart`

---

## Тестирование

### Запуск тестов

```bash
make test
# или
go test ./...
```

### Тестирование DNS-сервера

```bash
# Проверка разрешения домена
dig @127.0.0.1 -p 953 example.com

# Проверка IPv6
dig @127.0.0.1 -p 953 AAAA google.com

# Проверка блокировки
dig @127.0.0.1 -p 953 blocked-tracker.com
# Ожидается: 0.0.0.0
```

### Тестирование API

```bash
# Добавить домен
curl -X POST http://localhost:8090/domains -d "newdomain.com"

# Проверить
dig @127.0.0.1 -p 953 newdomain.com
# IP должен появиться в ipset
sudo ipset list vpn_domains
```

---

## Структура проекта

```
dns-box/
├── cmd/
│   └── dns-box/
│       └── main.go              # Точка входа, инициализация, graceful shutdown
├── internal/
│   ├── api/
│   │   ├── server.go            # HTTP API сервер
│   │   └── handlers.go          # Обработчики эндпоинтов
│   ├── blocklist/
│   │   └── blocklist.go         # Загрузка и управление блоклистами
│   ├── cache/
│   │   ├── domain_cache.go      # Кеш доменов и суффиксов (fastcache)
│   │   └── dns_cache.go         # Кеш DNS-запросов (fastcache)
│   ├── config/
│   │   └── config.go            # Загрузка, сохранение, мутации конфига
│   ├── dns/
│   │   ├── server.go            # DNS сервер (UDP/TCP)
│   │   └── handler.go           # Обработка DNS-запросов, резолвинг, ipset
│   ├── github/
│   │   └── client.go            # GitHub API клиент (загрузка/сохранение)
│   └── ipset/
│       └── ipset.go             # Обёртка над Linux ipset (только Linux)
├── config.json                  # Пример конфигурации
├── Makefile                     # Сборка и деплой
├── go.mod                       # Go модули
├── go.sum                       # Контрольные суммы зависимостей
└── README.md                    # Документация
```

---

## Логирование

### Уровни логирования

| Уровень | Описание |
|---------|----------|
| `trace` | Детальная информация о кеше и резолвинге |
| `debug` | Проверка доменов в правилах, добавление в ipset, блокировки |
| `info` | Запуск серверов, обновление блоклистов, загрузка из GitHub |
| `warn` | Ошибки upstream-серверов, проблемы с конфигурацией |
| `error` | Критические ошибки |

### Примеры логов

```
INFO[0000] DNS server started on 127.0.0.1:953
INFO[0000] Local config has no domains. Attempting to load from GitHub...
INFO[0000] Loaded 15 domains and 8 suffixes from GitHub
INFO[0000] Starting blocklist service...
INFO[0000] Updating blocklists...
INFO[0001] Loaded 12543 domains from https://blocklistproject.github.io/Lists/tracking.txt
INFO[0001] Blocklists updated successfully. Total domains: 12543
DEBUG[0002] Processing question: www.youtube.com.
DEBUG[0002] Domain matches suffix config, process: www.youtube.com (suffix: .youtube.com)
DEBUG[0002] Added IPv4 address 142.250.74.46 with timeout 300 for domain: www.youtube.com., to ipset: vpn_domains
```

---

## Лицензия

MIT
