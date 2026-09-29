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

# --- Нагрузочные тесты (ручные, в CI НЕ входят) ---------------------------
# hey не в go.mod (его зависимости конфликтуют с линтером, помнишь?) —
# ставится отдельно: go install github.com/rakyll/hey@latest
# Если нет в PATH — берём из ~/go/bin.
HEY ?= $(shell command -v hey 2>/dev/null || echo $(shell go env GOPATH)/bin/hey)

# Параметры по умолчанию: переопределяются с командной строки, напр.
#   make load-api LOAD_N=5000 LOAD_C=50
LOAD_N      ?= 300
LOAD_C      ?= 20
BENCH_TIME  ?= 15
BENCH_CLIENTS ?= 20
LOGIN_EMAIL ?= metrics@demo.dev
LOGIN_PASS  ?= Sup3rSecret!

.DEFAULT_GOAL := help
.PHONY: help migrate-up migrate-down migrate-status migrate-create test test-integration docs docs-check fmt fmt-check lint up down logs hooks load-setup load-api load-db

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

# --- Нагрузочные тесты ------------------------------------------------------
# РУЧНЫЕ инструменты: гоняются по требованию против ЛОКАЛЬНОГО стека
# (make up должен быть поднят). В CI их не пихают: они дорогие, шумные
# (заливают метрики тестовым трафиком) и требуют выделенной среды.

load-setup: ## Подготовить bench_db: создать + миграции + сид 100k юзеров (идемпотентно)
	@set -a; [ -f ./.env ] && . ./.env; set +a; \
	docker exec notes_db psql -U "$${POSTGRES_USER}" -d "$${POSTGRES_DB}" -c "CREATE DATABASE bench_db" 2>/dev/null || echo "bench_db уже существует"; \
	docker exec notes_api sh -c "GOOSE_DRIVER=postgres GOOSE_DBSTRING='postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@db:5432/bench_db?sslmode=disable' ./goose -dir ./migrations up" 2>&1 | tail -1; \
	docker exec -i notes_db psql -U "$${POSTGRES_USER}" -d bench_db -f - < scripts/pgbench_seed.sql

load-api: ## Нагрузочный тест API: hey на login (переменные: LOAD_N LOAD_C)
	@test -x "$(HEY)" || { echo "hey не найден: go install github.com/rakyll/hey@latest"; exit 1; }
	@set -a; [ -f ./.env ] && . ./.env; set +a; \
	$(HEY) -n $(LOAD_N) -c $(LOAD_C) -m POST \
		-H "Content-Type: application/json" \
		-d '{"email":"$(LOGIN_EMAIL)","password":"$(LOGIN_PASS)"}' \
		http://localhost:$${APP_PORT:-8080}/api/v1/auth/login

load-db: ## pgbench-нагрузка на bench_db (переменные: BENCH_TIME BENCH_CLIENTS)
	@set -a; [ -f ./.env ] && . ./.env; set +a; \
	docker cp scripts/pgbench_login.sql notes_db:/tmp/bench_login.sql >/dev/null; \
	docker exec notes_db pgbench -U "$${POSTGRES_USER}" -c $(BENCH_CLIENTS) -j 4 -T $(BENCH_TIME) -f /tmp/bench_login.sql bench_db