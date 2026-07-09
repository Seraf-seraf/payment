# Payment Service

HTTP-сервис для создания платежей, приема webhook от платежных провайдеров и доставки callback-событий мерчантам через outbox.

## Возможности

- создание платежей с идемпотентностью;
- аутентификация мерчантов по `X-API-Key` и HMAC-SHA256 подписи;
- интеграция с mock-провайдером и T-Bank;
- прием и дедупликация provider webhook;
- outbox worker для надежной доставки callback-событий;
- OpenAPI-first HTTP API;
- PostgreSQL-хранилище и миграции через goose;
- structured logging, trace id и Prometheus-метрики.

## Требования

- Go 1.26;
- PostgreSQL;
- `make`.

## Быстрый старт

1. Подготовить конфиг:

```bash
cp configs/config.example.yaml configs/config.yaml
```

2. Поднять PostgreSQL и проверить `postgres.dsn` в `configs/config.yaml`.

3. Накатить миграции:

```bash
make migrate-up
```

4. Запустить сервис:

```bash
make run
```

По умолчанию HTTP API доступен на `http://localhost:8080`.

Проверка:

```bash
curl http://localhost:8080/api/v1/health
curl http://localhost:8080/metrics
```

## Конфигурация

Основной путь к конфигу: `configs/config.yaml`. Его можно изменить через `CONFIG_PATH`.

Поддерживаемые env override:

- `APP_MODE`;
- `HTTP_ADDR`;
- `TBANK_TERMINAL_KEY`;
- `TBANK_PASSWORD`;
- `TBANK_NOTIFICATION_URL`;
- `SECURITY_ENCRYPTION_KEY`.

Пример локального запуска с другим адресом:

```bash
HTTP_ADDR=:8081 CONFIG_PATH=configs/config.yaml make run
```

## Миграции

Миграции лежат в `migrations/` и создают таблицы:

- `merchants`;
- `payments`;
- `webhook_events`;
- `outbox_events`.

Команды:

```bash
make migrate-up
make migrate-down
make migrate-status
```

DSN для goose берется из конфига через `cmd/configdsn`.

## API

OpenAPI-контракт: `api/openapi.yaml`.

Основные endpoints:

- `GET /api/v1/health` - health check;
- `POST /api/v1/payments` - создать платеж;
- `GET /api/v1/payments/{payment_id}` - получить платеж;
- `POST /api/v1/provider-webhooks/{provider_name}` - принять webhook провайдера;
- `GET /metrics` - Prometheus-метрики.

После изменения `api/openapi.yaml` нужно перегенерировать HTTP-код:

```bash
make generate
```

Команда также копирует спецификацию в `adapter/httpapi/openapi.yaml`, который embed'ится в бинарный файл.

## Подпись запросов мерчанта

`POST /api/v1/payments` требует заголовки:

- `X-API-Key` - API key мерчанта;
- `X-Timestamp` - Unix timestamp в секундах;
- `X-Signature` - hex-encoded HMAC-SHA256;
- `Idempotency-Key` - опциональный ключ идемпотентности.

Сообщение для подписи:

```text
<X-Timestamp>.<raw request body>
```

Ключ подписи - `shared_secret` мерчанта. Допустимое расхождение времени задается `security.hmac_max_skew`.

Пример:

```bash
body='{"order_id":"order-1","amount_minor":10000,"currency":"RUB","description":"Test payment","receipt":{"email":"customer@example.com","taxation":"osn","items":[{"name":"Test payment","price_minor":10000,"quantity":1,"amount_minor":10000,"payment_method":"full_payment","payment_object":"service","tax":"none"}]}}'
ts="$(date +%s)"
signature="$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$SHARED_SECRET" -hex | awk '{print $2}')"

curl -X POST http://localhost:8080/api/v1/payments \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $API_KEY" \
  -H "X-Timestamp: $ts" \
  -H "X-Signature: $signature" \
  -H "Idempotency-Key: order-1" \
  -d "$body"
```

## Провайдеры

Провайдер по умолчанию задается в `providers.default`.

- `mock` включен в example-конфиге и подходит для локальной разработки;
- `tbank` выключен по умолчанию, для работы нужны `terminal_key`, `password` и `notification_url`.

Provider webhook принимается по адресу:

```text
POST /api/v1/provider-webhooks/{provider_name}
```

Для T-Bank успешный ответ возвращается как plain text `OK`.

## Outbox и callback

При обработке webhook сервис обновляет статус платежа и создает событие в `outbox_events`. Outbox worker выбирает pending-события и отправляет callback на `callback_url` мерчанта.

Настройки worker находятся в секции `outbox`:

- `enabled`;
- `poll_interval`;
- `batch_size`;
- `max_attempts`;
- `worker_count`.

Таймаут исходящего callback задается в `callback.timeout`.

## Команды разработки

Все основные команды описаны в `Makefile`:

```bash
make help
make tidy
make fmt
make check
make test
make build
make run
make generate
make clean
```

Перед отправкой изменений полезно запускать:

```bash
make check
make test
```

## Структура проекта

- `cmd/api` - точка входа HTTP-сервиса;
- `cmd/configdsn` - вывод PostgreSQL DSN из конфига для миграций;
- `api/openapi.yaml` - публичный OpenAPI-контракт;
- `adapter/httpapi` - HTTP handlers, router, OpenAPI-валидация и generated-код;
- `adapter/provider` - интеграции с платежными провайдерами;
- `adapter/storage/postgres` - PostgreSQL-репозитории и транзакции;
- `adapter/notification` - доставка callback;
- `domain` - доменные модели;
- `service` - бизнес-сценарии;
- `ports` - интерфейсы между слоями;
- `pkg` - общие пакеты;
- `bootstrap` - сборка зависимостей приложения;
- `migrations` - SQL-миграции.
