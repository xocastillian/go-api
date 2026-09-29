#!/bin/sh
# ===========================================================================
# entrypoint.sh — точка входа контейнера api.
#
# Docker запускает этот скрипт как ENTRYPOINT. Он делает то, что не вписать
# в одну команду: собирает DSN, накатывает миграции и лишь потом стартует API.
#
# Shell здесь — /bin/sh (BusyBox ash в alpine), обычный POSIX shell.
# ТРЕБОВАНИЯ к файлу: LF-переносы строк (не CRLF!) и бит исполнения (+x).
# Файл с Windows-переносами даст загадочное "no such file or directory:
# ./entrypoint.sh" — shell ищет интерпретатор с именем "sh\r".
# ===========================================================================

# -e — «упасть при первой же ошибке». Без этого провал миграций прошёл бы
# незамеченным, и API стартанул бы на неготовой БД. Это fail fast.
set -e

# --- 1. Собираем DSN для goose ---
# Берём ТЕ ЖЕ POSTGRES_*, что читает приложение (config.Load). Второй копии
# строки подключения не заводим — источник правды один. Значения по умолчанию
# совпадают с config.Load(): host=localhost (здесь переопределён на db в
# docker-compose), port=5432, sslmode=disable (в проде — POSTGRES_SSLMODE).
: "${POSTGRES_HOST:=db}"
: "${POSTGRES_PORT:=5432}"
: "${POSTGRES_SSLMODE:=disable}"
DSN="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@${POSTGRES_HOST}:${POSTGRES_PORT}/${POSTGRES_DB}?sslmode=${POSTGRES_SSLMODE}"

# --- 2. Накатываем миграции ---
# Идемпотентно: goose применённые миграции пропускает. Так что при каждом
# старте безопасно. (Postgres уже поднят — этого ждёт depends_on.condition
# service_healthy в docker-compose.)
echo "[entrypoint] applying migrations..."
GOOSE_DRIVER=postgres GOOSE_DBSTRING="$DSN" ./goose -dir ./migrations up

# --- 3. Запускаем API ---
echo "[entrypoint] starting api..."
# exec ЗАМЕНЯЕТ процесс shell на api (а не порождает дочерний).
# Благодаря этому api становится PID 1 и получает сигналы (SIGTERM от
# `docker stop`) НАПРЯМУЮ — иначе сигнал пришёл бы shell'у, который его не
# пересылает, и graceful shutdown сервера не сработал бы.
exec ./api