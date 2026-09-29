package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool создаёт пул соединений к Postgres, проверяет живой коннект
// и возвращает готовый пул либо ошибку.
// ctx — контекст, чтобы можно было отменить/затаймаутить создание.
// dsn — строка подключения (postgres://user:pass@host:port/db?...).
// maxConns/minConns — размеры пула, приходят из config (env PGX_MAX_CONNS/
// PGX_MIN_CONNS): раньше были захардкожены, но цифры такого рода
// подбираются замером и меняются без пересборки кода.
func NewPool(ctx context.Context, dsn string, maxConns, minConns int32) (*pgxpool.Pool, error) {
	// ParseConfig разбирает DSN в структуру настроек пула.
	// Возвращает cfg, которую можно донастроить перед созданием пула.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// Оборачиваем ошибку с контекстом "где упало" и возвращаем её наверх.
		return nil, fmt.Errorf("parse pg config: %w", err)
	}

	// Максимум одновременных соединений к БД.
	// Защищает Postgres от перегрузки: лишние запросы ждут в очереди пула.
	cfg.MaxConns = maxConns
	// Держим минимум minConns соединений «наготове», чтобы не открывать коннект на каждый запрос.
	cfg.MinConns = minConns
	// Через час закрываем соединение и открываем новое — чтобы не держать «протухшие».
	cfg.MaxConnLifetime = time.Hour
	// Простаивающее соединение закрываем через 30 минут — не держим лишние ресурсы.
	cfg.MaxConnIdleTime = 30 * time.Minute

	// Создаём сам пул по настроенной cfg. Wait — ошибки конфигурации/создания.
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// Делаем производный контекст с таймаутом 5с,
	// чтобы Ping не завис навсегда, если БД недоступна.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	// Обязательно освобождаем таймер контекста, иначе утечка. Отложенный вызов.
	defer cancel()

	// Пробуем реально дёрнуть БД. fail fast: узнаём о проблеме на старте,
	// а не при первом пользовательском запросе.
	if err := pool.Ping(pingCtx); err != nil {
		// Пул уже создан — закрываем его, иначе утечка соединений.
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	// Отдаём готовый пул наружу. Закроет его вызывающий (defer pool.Close()).
	return pool, nil
}
