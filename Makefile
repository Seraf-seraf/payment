APP_NAME ?= payment-service
HTTP_ADDR ?= :8080
BIN_DIR ?= bin
BIN_PATH := $(BIN_DIR)/$(APP_NAME)
OPENAPI_SPEC ?= api/openapi.yaml
OAPI_CONFIG ?= oapi-codegen.yaml
CONFIG_PATH ?= configs/config.yaml

.PHONY: help tidy fmt check test build run generate migrate-up migrate-down migrate-status clean

help: ## Показать список доступных команд
	@awk 'BEGIN {FS = ":.*##"; printf "Доступные команды:\n"} /^[a-zA-Z_-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

tidy: ## Синхронизировать go.mod и go.sum
	go mod tidy

fmt: ## Отформатировать Go-код
	gofmt -w ./cmd ./adapter ./app ./bootstrap ./domain ./pkg ./ports ./service

check: ## Проверить go mod tidy, gofmt и go vet
	go mod tidy
	gofmt -w ./cmd ./adapter ./app ./bootstrap ./domain ./pkg ./ports ./service
	go vet ./...

test: ## Запустить тесты
	go test ./...

build: ## Собрать бинарный файл сервиса
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_PATH) ./cmd/api

run: ## Запустить HTTP API локально
	CONFIG_PATH=$(CONFIG_PATH) HTTP_ADDR=$(HTTP_ADDR) go run ./cmd/api

generate: ## Сгенерировать Go-код из OpenAPI-спецификации
	cp $(OPENAPI_SPEC) adapter/httpapi/openapi.yaml
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen --config $(OAPI_CONFIG) $(OPENAPI_SPEC)

migrate-up: ## Накатить миграции БД
	go run github.com/pressly/goose/v3/cmd/goose -dir migrations postgres "$$(go run ./cmd/configdsn -config $(CONFIG_PATH))" up

migrate-down: ## Откатить последнюю миграцию БД
	go run github.com/pressly/goose/v3/cmd/goose -dir migrations postgres "$$(go run ./cmd/configdsn -config $(CONFIG_PATH))" down

migrate-status: ## Показать статус миграций БД
	go run github.com/pressly/goose/v3/cmd/goose -dir migrations postgres "$$(go run ./cmd/configdsn -config $(CONFIG_PATH))" status

clean: ## Удалить артефакты сборки
	rm -rf $(BIN_DIR)
