// Package testsupport — общие помощники для ИНТЕГРАЦИОННЫХ тестов.
// Импортируется только из *_test.go, поэтому в прод-бинарь не попадает.
package testsupport

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/timqa/my-rest-api/internal/config"
)

// NewPool подключается к БД и возвращает пул. Если БД недоступна или
// не настроена — тест ПРОПУСКАЕТСЯ (Skip), а не падает: интеграционные
// тесты требуют живой Postgres, и без него их гонять бессмысленно.
//
// Пул закрывается автоматически через t.Cleanup — вручную Close писать не надо.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		// Разрешаем переопределить строку подключения через TEST_DATABASE_URL
		// (удобно в CI). Иначе берём те же настройки, что и dev-сервер из .env.
		loadRootEnv(t)
		cfg, err := config.Load()
		if err != nil {
			t.Skipf("интеграционные тесты пропущены: конфиг БД недоступен (%v)", err)
		}
		dsn = cfg.Postgres.DSN()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("создать пул: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("интеграционные тесты пропущены: БД недоступна (%v)", err)
	}

	// t.Cleanup выполнится по завершении теста — закроет пул за нас.
	t.Cleanup(pool.Close)
	return pool
}

// loadRootEnv находит корень модуля (по go.mod) и грузит оттуда .env.
//
// Зачем: `go test` запускает тест с рабочей директорией = папка пакета
// (например, internal/user), а .env лежит в корне. config.Load() ищет
// .env в текущей директории и промахнулся бы. Поэтому идём вверх по дереву.
func loadRootEnv(t *testing.T) {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			// Нашли корень модуля — грузим .env рядом с go.mod.
			// Ошибку игнорируем: .env может отсутствовать (тогда ждём env из окружения).
			_ = godotenv.Load(filepath.Join(dir, ".env"))
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return // дошли до корня файловой системы, .env не найден
		}
		dir = parent
	}
}
