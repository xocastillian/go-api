-- +goose Up
-- Итоговая схема проекта. Проект поднимается "с нуля", поэтому вся схема
-- живёт в одной миграции в своём финальном виде — без промежуточных
-- ALTER'ов, которые были нужны только для эволюции на живых данных.

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Уникальность email — регистронезависимая. Функциональный индекс по
-- lower(email) надёжнее UNIQUE-колонки: "A@B.com" конфликтует с "a@b.com"
-- на любом пути записи, даже если код-путь нормализацию обошёл.
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

CREATE TABLE notes (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title      TEXT NOT NULL,
    content    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_notes_user_id ON notes(user_id);

-- Refresh-токены: долгоживущие "пропуска" для получения новых access-токенов.
-- Храним не сам токен, а его sha256-хеш: утечка БД не даёт рабочих токенов.
CREATE TABLE refresh_tokens (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,          -- NULL = токен "живой"
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_refresh_tokens_user_id ON refresh_tokens(user_id);

-- Функция автоматически проставляет updated_at при любом UPDATE.
-- StatementBegin/StatementEnd ОБЯЗАТЕЛЬНЫ: тело функции содержит ';',
-- которые goose иначе принял бы за конец оператора.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Одна функция — несколько таблиц: логика описана один раз.
CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER notes_set_updated_at
    BEFORE UPDATE ON notes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
-- Порядок обратен созданию: сначала таблицы, потом функция.
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS notes;
DROP TABLE IF EXISTS users;
DROP FUNCTION IF EXISTS set_updated_at();