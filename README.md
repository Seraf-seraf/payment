# Payment Service

HTTP-сервис для создания платежей, приема webhook от платежных провайдеров и доставки callback-событий мерчантам через outbox.

## Возможности

- создание платежей с идемпотентностью;
- аутентификация мерчантов по `X-API-Key` и HMAC-SHA256 подписи;
- интеграция с mock-провайдером, T-Bank и CloudPayments;
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
- `CLOUDPAYMENTS_PUBLIC_ID`;
- `CLOUDPAYMENTS_API_SECRET`;
- `SECURITY_ENCRYPTION_KEY`.

`app.mode` (или env override `APP_MODE`) выбирает компоненты процесса:

- `all` — HTTP API и включённые фоновые workers; это режим по умолчанию;
- `api` — только HTTP API;
- `worker` — только включённые фоновые workers, без HTTP listener.

Callback worker запускается при `outbox.enabled`; invoice worker — при `invoices.enabled` и `invoices.worker_enabled`. Режим `all` сам по себе не включает отключённый worker.

Пример локального запуска с другим адресом:

```bash
HTTP_ADDR=:8081 CONFIG_PATH=configs/config.yaml make run
```

## CloudPayments

Секция `providers.cloudpayments` содержит `enabled` (по умолчанию `false`),
`api_url` (`https://api.cloudpayments.ru`), `public_id`, `api_secret`,
`test_mode` (`true`) и `timeout` (`10s`). Непустые `CLOUDPAYMENTS_PUBLIC_ID`
и `CLOUDPAYMENTS_API_SECRET` заменяют credentials из YAML; остальные параметры
задаются в YAML. Секреты храните вне репозитория.

Для выбора адаптера у соответствующего мерчанта используется существующее
`Merchant.ProviderName = "cloudpayments"`. Включение адаптера не переключает
мерчантов автоматически.

| `payment_method` | Создание ссылки | Результат |
| --- | --- | --- |
| `card`, `bank_card`, пустое значение | `/orders/create` | Карточная платёжная страница |
| `sbp` | `/payments/qr/sbp/link` | Прямая СБП-ссылка |

Поддерживается RUB; сумма передаётся из целых копеек точно. Оба способа возвращают
существующий `payment_url` со статусом `pending`; создание ссылки ещё не означает
оплату. Страница может показывать другие способы, разрешённые терминалом.
`InvoiceId` — внутренний UUID попытки платежа, а не номер заказа.

Для одностадийного терминала ожидаются POST-уведомления **Pay и Fail** на
`/api/v1/provider-webhooks/cloudpayments` в UTF-8 JSON или form-urlencoded.
Check, Confirm, Refund, Cancel и Recurrent на этот endpoint не направляются.
Проверяется `Content-HMAC` по исходным байтам; `X-Content-HMAC` не используется.
`test_mode` должен совпадать с режимом терминала и уведомлений: для карточной
страницы он не переключает терминал, а для СБП передаётся как `IsTest`.
Это справка по ожидаемым настройкам, а не автоматическая настройка терминала.

Pay со статусом Completed завершает оплату. Fail отдельной банковской попытки
оставляет сессию `pending`, позволяя последующий Pay; поздний Fail не откатывает
успех. Ответ `200 {"code":0}` выдаётся после обработки или идемпотентного повтора;
доставка callback мерчанту остаётся асинхронной через outbox. При ошибке обработчик
возвращает ненулевой code; OpenAPI middleware может отклонить запрос раньше со
своим общим форматом ошибки. Если платёж ещё создаётся, уведомление не подтверждается.

Существующий чек преобразуется в `CustomerReceipt` внутри запроса оплаты.
Отдельная кассовая интеграция, реальные списания, возвраты, настройка терминала
и согласование срока ссылки со сроком резерва в этот адаптер не входят.
Локальные тесты используют HTTP-дубль и изолированный PostgreSQL: они не подтверждают
работу подключённого терминала или выдачу фискального чека.

## Миграции

Миграции лежат в `migrations/` и создают таблицы:

- `merchants`;
- `payments`;
- `webhook_events`;
- `outbox_events`;
- `invoice_payments`;
- `invoice_evidence`;
- `invoice_inbox`;
- `invoice_commands`.

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

`make generate` использует `api/openapi.yaml` и настройки `oapi-codegen.yaml`. Команда копирует спецификацию в `adapter/httpapi/openapi.yaml`, которая встраивается в бинарный файл для OpenAPI-валидации, и создаёт `adapter/httpapi/generated.gen.go`: модели запросов и ответов, enum, интерфейсы handlers, маршрутизацию Chi и strict-server обёртки. Generated Go-файл обновляется через спецификацию и генератор; бизнес-логика и SQL-миграции не генерируются.

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

## Внутренний сценарий invoice

Invoice для юрлица использует отдельные операции:

- `POST /api/v1/invoice-payments` — сохранить намерение и поставить выставление в очередь; первая фиксация возвращает 202, повтор — сохранённую проекцию;
- `GET /api/v1/invoice-payments/{payment_id}` — получить состояние своего invoice;
- `POST /api/v1/provider-invoice-webhooks/{connection_id}` — принять проверенное уведомление подключения.

Создание требует `Idempotency-Key`, равный UUID `request_id`, и обычную merchant HMAC-подпись исходных байтов. Денежные величины передаются целыми minor units. Для GET подписывается `<X-Timestamp>.`. Схемы запросов и ответов определены в `api/openapi.yaml`; acquiring endpoint по-прежнему требует `receipt`.

Секция `invoices` в example-конфиге выключена. `mock_invoice` разрешён только для local/test, не создаёт реальный банковский счёт и возвращает реквизиты для `bank_transfer` без выдуманного URL. Его webhook требует HMAC-SHA256 исходного тела в `X-Mock-Signature`; имя env secret задаётся в подключении. Реальный банк этим сценарием не подключён.

Входящие факты хранятся в `invoice_inbox`, внутренние задания — в отдельной `invoice_commands`. Уведомление с несколькими разными reference сохраняется для review без начисления денег; повтор одного reference обрабатывается как одна ссылка. Существующий callback worker читает только `outbox_events`; invoice callback содержит сохранённый snapshot `invoice-integration/1`. Payment учитывает финансовое покрытие и review, а резерв и статусы исполнения заказа принадлежат магазину.

Нормализация запроса, request hash и правила финансового покрытия принадлежат `domain/invoice`. `service/invoice` координирует транзакции, durable дедупликацию evidence, lease fencing и сохранение callback snapshot. Денежное покрытие применяется только после результата дедупликации; повтор не увеличивает сумму или revision.

### Проверки с PostgreSQL

```bash
make test-u
make test-fn
go test -race ./service/invoice ./adapter/provider/mockinvoice ./adapter/notification/httpcallback
go vet ./...
make build
make generate
cmp api/openapi.yaml adapter/httpapi/openapi.yaml
```

Для `test-fn` нужен работающий Docker. Testcontainers создаёт PostgreSQL `postgres:16.13-alpine3.23` и БД `payment_test` при старте suite; каждый тест получает отдельную schema со всеми миграциями. DSN приходит только из контейнера: `CONFIG_PATH` и внешняя БД для этих тестов не используются. Ошибка запуска контейнера завершает suite с ошибкой, без Skip. После тестов удаляются их schemas и контейнер.

`test-u` запускает unit-тесты без Docker; `test-fn` — PostgreSQL storage, HTTP, worker и callback сценарии с build tag `functional` без test cache. `make test` сохранён как alias для `test-u`. Это локальные gates, они не меняют production deploy или CI.

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

Для проверки изменений без форматирования всего репозитория и изменения зависимостей:

```bash
make test-u
make test-fn
go vet ./...
```

`make check` и `make fmt` форматируют весь репозиторий; `make check` также запускает `go mod tidy`. Для проверки узкого diff форматируйте только изменённые Go-файлы.

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
