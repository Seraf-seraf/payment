APP_NAME ?= go-swagger-template
HTTP_ADDR ?= :8080
BIN_DIR ?= bin
BIN_PATH := $(BIN_DIR)/$(APP_NAME)
OPENAPI_SPEC ?= api/openapi.yaml
OAPI_CONFIG ?= oapi-codegen.yaml

.PHONY: help tidy fmt check test build run generate clean

help: ## Показать список доступных команд
	@awk 'BEGIN {FS = ":.*##"; printf "Доступные команды:\n"} /^[a-zA-Z_-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

tidy: ## Синхронизировать go.mod и go.sum
	go mod tidy

fmt: ## Отформатировать Go-код
	gofmt -w $$(git ls-files '*.go')

check: ## Проверить go mod tidy, gofmt и go vet
	go mod tidy
	gofmt -w ./cmd ./internal
	go vet ./...

test: ## Запустить тесты
	go test ./...

build: ## Собрать бинарный файл сервиса
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_PATH) ./cmd/api

run: ## Запустить HTTP API локально
	HTTP_ADDR=$(HTTP_ADDR) go run ./cmd/api

generate: ## Сгенерировать Go-код из OpenAPI-спецификации
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen --config $(OAPI_CONFIG) $(OPENAPI_SPEC)

clean: ## Удалить артефакты сборки
	rm -rf $(BIN_DIR)
