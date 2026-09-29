package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/timqa/my-rest-api/internal/apperr"
	"github.com/timqa/my-rest-api/internal/platform/postgres"
)

// Repository — доступ к таблице refresh_tokens.
// Работает через postgres.DBTX, поэтому одинаково годится и для пула
// (обычная работа), и для транзакции (интеграционные тесты с откатом).
// Отвечает ТОЛЬКО за SQL. Определять, "валиден ли токен", — дело сервиса:
// репозиторий отдаёт запись как есть, а бизнес-правила живут выше.
type Repository struct {
	db postgres.DBTX
}

// NewRepository — конструктор, DBTX приходит снаружи (DI).
func NewRepository(db postgres.DBTX) *Repository {
	return &Repository{db: db}
}

// refreshRecord — строка таблицы refresh_tokens.
// Не экспортируем: это внутренняя деталь пакета, наружу отдаём через сервис.
type refreshRecord struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time // nil = токен "живой"; non-nil = отозван
	CreatedAt time.Time
}

// Create сохраняет новый refresh-токен.
// Принимает УЖЕ ГОТОВЫЙ хеш (а не сырой токен): хеширование — забота refresh.go,
// поэтому сырой токен физически не может попасть в БД.
func (r *Repository) Create(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	const query = `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`

	if _, err := r.db.Exec(ctx, query, userID, tokenHash, expiresAt); err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

// GetByHash ищет токен по его хешу (клиент прислал токен → мы захешировали → ищем).
// Возвращает запись целиком: решение "протух/отозван" принимает сервис.
func (r *Repository) GetByHash(ctx context.Context, tokenHash string) (refreshRecord, error) {
	const query = `
		SELECT id, user_id, token_hash, expires_at, revoked_at, created_at
		FROM refresh_tokens
		WHERE token_hash = $1
	`
	return scanRefreshRecord(r.db.QueryRow(ctx, query, tokenHash))
}

// GetByHashForUpdate — то же самое, но с БЛОКИРОВКОЙ строки.
//
// Хвост "FOR UPDATE" говорит Postgres: заблокируй найденную строку на время
// транзакции. Пока наша транзакция не закончится, второй читатель, который
// тоже попросит FOR UPDATE по этой строке, ЗАВИСНЕТ и подождёт.
//
// Зачем: Rotate — "прочитал → отозвал → создал новый". Два конкурентных
// refresh-запроса с ОДНИМ токеном без блокировки оба прочитали бы "живой",
// и из одного токена родились бы ДВЕ сессии. С FOR UPDATE второй запрос
// подождёт коммита первого, увидит revoked_at != nil и честно пойдёт по
// пути детекта кражи.
//
// Метод имеет смысл ТОЛЬКО внутри транзакции (WithTx): блокировка живёт
// до конца транзакции. Вне WithTx это просто обычный SELECT.
func (r *Repository) GetByHashForUpdate(ctx context.Context, tokenHash string) (refreshRecord, error) {
	const query = `
		SELECT id, user_id, token_hash, expires_at, revoked_at, created_at
		FROM refresh_tokens
		WHERE token_hash = $1
		FOR UPDATE
	`
	return scanRefreshRecord(r.db.QueryRow(ctx, query, tokenHash))
}

// scanRefreshRecord — общий скан одной строки refresh_tokens для GetByHash
// и GetByHashForUpdate: SQL у них разный, а набор колонок и маппинг один.
func scanRefreshRecord(row pgx.Row) (refreshRecord, error) {
	var rec refreshRecord
	err := row.Scan(&rec.ID, &rec.UserID, &rec.TokenHash, &rec.ExpiresAt, &rec.RevokedAt, &rec.CreatedAt)
	if err != nil {
		// Нет строки → переводим в доменную "не найдено".
		// Сервис превратит это в "невалидный refresh-токен".
		if errors.Is(err, pgx.ErrNoRows) {
			return refreshRecord{}, fmt.Errorf("get refresh token: %w", apperr.ErrNotFound)
		}
		return refreshRecord{}, fmt.Errorf("get refresh token: %w", err)
	}
	return rec, nil
}

// WithTx выполняет fn атомарно: в ОДНОЙ транзакции БД.
//
// Контракт:
//   - fn получает sessionStore, который работает ВНУТРИ этой транзакции —
//     все его Create/Revoke/... пойдут через tx, а не через пул;
//   - fn вернул nil            → транзакция КОММИТИТСЯ (все изменения сохранены);
//   - fn вернул ошибку         → транзакция ОТКАТЫВАЕТСЯ (как будто ничего не было);
//   - fn запаниковал           → тоже откат (defer ниже выполнится при размотке),
//     а паника летит дальше к вызывающему — мы её не глушим.
//
// Почему контракт именно такой — классическая проблема "перевода денег":
// операция из двух зависимых шагов без транзакции может остановиться
// посередине, оставив БД в несогласованном состоянии (списано, но не
// зачислено). Транзакция делает её неделимой.
//
// Аргумент fn имеет тип sessionStore — не *Repository: сервису не нужно
// знать, что внутри транзакции лежит конкретный тип. *Repository
// удовлетворяет интерфейсу структурно, поэтому tx-обёртка подставляется
// без всяких адаптеров.
func (r *Repository) WithTx(ctx context.Context, fn func(sessionStore) error) error {
	// Открываем транзакцию через r.db — там ПУЛ (или, если репозиторий
	// уже на транзакции, вложенная через SAVEPOINT — см. dbtx.go).
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Страховка от "забыл закрыть транзакцию": пока committed == false,
	// defer откатывает tx — при ЛЮБОМ выходе: ошибка из fn, panic,
	// провалившийся Commit. Флаг ставится только после успешного Commit,
	// поэтому незакрытых транзакций и утечек соединений не бывает.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Копия репозитория, сидящая на транзакции. Тот же тип *Repository —
	// все методы работают как обычно, но каждый запрос идёт внутри tx.
	if err := fn(&Repository{db: tx}); err != nil {
		return err // defer откатит транзакцию
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}

// Revoke помечает конкретный токен отозванным (logout или ротация).
// Идемпотентно: повторный отзыв не ошибка — просто перепишет revoked_at.
// COALESCE сохраняет ПЕРВЫЙ момент отзыва, если вызвать повторно.
func (r *Repository) Revoke(ctx context.Context, id uuid.UUID) error {
	const query = `
		UPDATE refresh_tokens
		SET revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1
	`

	if _, err := r.db.Exec(ctx, query, id); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

// RevokeAllForUser отзывает ВСЕ живые токены пользователя.
// Нужен при детекте кражи (кто-то переиспользовал старый токен) и для logout-all.
// Условие revoked_at IS NULL — повторный вызов не перезапишет старые даты.
func (r *Repository) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	const query = `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
	`

	if _, err := r.db.Exec(ctx, query, userID); err != nil {
		return fmt.Errorf("revoke all refresh tokens: %w", err)
	}
	return nil
}

// DeleteExpired удаляет протухшие refresh-токены (expires_at < now)
// и возвращает число удалённых строк.
//
// Правило чистки — ТОЛЬКО протухшие, и вот почему:
//   - ОТЗЫВАННЫЙ, но ещё не протухший токен удалять НЕЛЬЗЯ: на его записи
//     держится детект кражи (Rotate видит revoked_at != nil → kill all).
//     Удалим раньше срока — вор получит "not found" вместо детекта.
//   - ПРОТУХШИЙ токен бесполезен по определению: Rotate отбросил бы его и
//     так (проверка ExpiresAt). Его удаление ничего не меняет в логике,
//     только разгружает таблицу: диск, индексы, бэкапы, приватность.
//
// without now-аргумента: время передаёт вызывающий (сервис), а не SQL now().
// Так метод детерминирован и тестируем: в тесте можно подставить любое "сейчас".
func (r *Repository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	const query = `
		DELETE FROM refresh_tokens
		WHERE expires_at < $1
	`

	tag, err := r.db.Exec(ctx, query, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired refresh tokens: %w", err)
	}
	// RowsAffected здесь и есть результат: сколько строк убрали.
	return tag.RowsAffected(), nil
}
