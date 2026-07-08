# go_swagger_template

Масштабируемый Go-шаблон HTTP API с OpenAPI-first подходом, структурированным логированием, трассировкой запросов и Prometheus-метриками.

## Библиотеки

- `github.com/go-chi/chi/v5` — основной HTTP-роутер, совместимый со стандартным `net/http`; используется для регистрации маршрутов, группировки API, версионирования роутов и подключения middleware.
- `github.com/justinas/alice` — сборка middleware-цепочек без ручного вложения обработчиков друг в друга.
- `github.com/prometheus/client_golang/prometheus` — объявление и регистрация counters, gauges, histograms, business metrics и технических метрик.
- `github.com/prometheus/client_golang/prometheus/promhttp` — endpoint `/metrics` и инструментирование HTTP-handler'ов.
- `github.com/go-chi/httplog/v3` — production-ready structured request logging через стандартный `log/slog`.
- `github.com/go-chi/traceid` — сквозной trace id через HTTP-заголовок и context без обязательного подключения OpenTelemetry.
- `github.com/oapi-codegen/nethttp-middleware` — валидация входящих HTTP-запросов по OpenAPI-спецификации до бизнес-логики.
- `github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen` — CLI для генерации Go-типов, server interfaces, strict handlers и клиентов из `api/openapi.yaml`.

## Быстрый старт

```bash
go run ./cmd/api
```

Проверка API:

```bash
curl http://localhost:8080/api/v1/health
curl http://localhost:8080/metrics
```

## Структура

- `api/openapi.yaml` — единый контракт API.
- `cmd/api` — точка входа HTTP-сервиса.
- `internal/httpapi` — роутинг, middleware, handler'ы, метрики и OpenAPI-валидация.
- `oapi-codegen.yaml` — конфигурация генерации кода из OpenAPI.
