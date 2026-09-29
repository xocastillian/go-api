# Makefile — короткие команды для рутины. Секреты и настройки БД живут в .env,
# здесь только их использование. Ничего в .env добавлять НЕ нужно: DSN для
# goose собирается из тех же POSTGRES_*, что читает config.Load(), поэтому
# источник правды ровно один.
#
# Запуск: make (или make help) — покажет список команд.

SHELL := /bin/sh

# --- Переменные ------------------------------------------------------------

# Строка подключения для goose. $$ -> $ для шелла (Make сам ест один $).
# $${VAR:-default} — подставляем те же дефолты, что и config.Load(),
# на случай если в .env переменной нет. sslmode — тот же POSTGRES_SSLMODE.
DB_DSN = postgres://$$POSTGRES_USER:$$POSTGRES_PASSWORD@$${POSTGRES_HOST:-localhost}:$${POSTGRES_PORT:-5432}/$$POSTGRES_DB?sslmode=$${POSTGRES_SSLMODE:-disable}

# Общая обёртка: подгружаем .env в окружение шелла и запускаем goose.
# `set -a` авто-экспортирует переменные из .env — их увидят дочерние процессы.
# `[ -f ./.env ] &&` — загружаем файл ТОЛЬКО если он есть: в CI .env нет,
# все POSTGRES_* приходят из environment джобы, и источник правды тот же.
GOOSE = set -a; [ -f ./.env ] && . ./.env; set +a; GOOSE_DRIVER=postgres GOOSE_DBSTRING="$(DB_DSN)" go tool goose -dir db/migrations

.DEFAULT_GOAL := help
.PHONY: help migrate-up migrate-down migrate-status migrate-create test test-integration docs docs-check fmt fmt-check lint up down logs hooks

# --- Команды ---------------------------------------------------------------

help: ## Список доступных команд
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

migrate-up: ## Накатить все неприменённые миграции
	@$(GOOSE) up

migrate-down: ## Откатить последнюю миграцию
	@$(GOOSE) down

migrate-status: ## Показать, что применено
	@$(GOOSE) status

migrate-create: ## Создать миграцию: make migrate-create name=add_widgets
	@go tool goose -dir db/migrations create $(name) sql

docs: ## Перегенерировать Swagger-документацию в docs/
	@go tool swag init -g cmd/api/main.go -o docs --parseInternal --parseDependency --useStructName

fmt: ## Форматировать Go-код (gofmt -w ./cmd ./internal)
	gofmt -w ./cmd ./internal

fmt-check: ## Проверить форматирование, НЕ меняя файлы (для CI)
	@out=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$out" ]; then \
		echo "файлы не отформатированы (запусти make fmt):"; echo "$$out"; exit 1; \
	fi

docs-check: ## Перегенерировать Swagger и упасть, если docs/ устарели
	@go tool swag init -g cmd/api/main.go -o docs --parseInternal --parseDependency --useStructName >/dev/null
	@git diff --exit-code -- docs >/dev/null || \
		{ echo "docs/ неактуальны: поменялись аннотации Swagger — запусти make docs и закоммить"; exit 1; }

lint: ## Проверить код линтером (golangci-lint, конфиг .golangci.yml)
	go tool golangci-lint run

test: ## Unit-тесты (БД не нужна)
	go test ./...

test-integration: ## Интеграционные тесты (нужна живая БД)
	go test -tags integration -count=1 ./...

up: ## Поднять db + api в докере (миграции применятся автоматически)
	docker compose up -d --build

down: ## Остановить контейнеры (данные БД сохраняются в томе)
	docker compose down

logs: ## Смотреть логи api в реальном времени
	docker compose logs -f api

hooks: ## Установить pre-commit хуки (нужен lefthook: brew install lefthook)
	lefthook install