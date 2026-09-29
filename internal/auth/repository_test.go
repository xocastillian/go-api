//go:build integration

// Интеграционные тесты репозитория auth (таблица refresh_tokens).
// Запуск: go test -tags integration ./internal/auth/
package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timqa/my-rest-api/internal/apperr"
	"github.com/timqa/my-rest-api/internal/platform/postgres"
	"github.com/timqa/my-rest-api/internal/testsupport"
)

func newTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()

	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// insertUser заводит пользователя СЫРЫМ SQL.
// Мы не можем импортировать пакет user (был бы цикл: auth→user→auth),
// поэтому вставляем строку сами — нам нужен только id для FK.
//
// Аргумент — postgres.DBTX, а не pgx.Tx: и транзакция, и пул удовлетворяют
// ему структурно. С транзакцией пользователь "зависнет" до отката теста,
// с пулом — закоммитится сразу (нужно тестам WithTx, где данные должны
// пережить открываемые внутри транзакции).
func insertUser(t *testing.T, db postgres.DBTX) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := db.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, role) VALUES ($1, 'hash', 'user') RETURNING id`,
		"auth-test-"+uuid.NewString()+"@test.local",
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// createToken создаёт токен и возвращает его хеш (по нему потом ищем в БД).
func createToken(t *testing.T, repo *Repository, userID uuid.UUID) string {
	t.Helper()

	hash := "hash-" + uuid.NewString()
	if err := repo.Create(context.Background(), userID, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Create token: %v", err)
	}
	return hash
}

func TestRepository_CreateAndGetByHash(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	tx := newTx(t, pool)
	repo := NewRepository(tx)

	userID := insertUser(t, tx)
	hash := createToken(t, repo, userID)

	rec, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if rec.UserID != userID {
		t.Errorf("user_id: получили %v, ждали %v", rec.UserID, userID)
	}
	if rec.TokenHash != hash {
		t.Errorf("token_hash: получили %q", rec.TokenHash)
	}
	if rec.RevokedAt != nil {
		t.Errorf("свежесозданный токен не должен быть отозван")
	}
}

func TestRepository_GetByHash_NotFound(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(newTx(t, pool))

	_, err := repo.GetByHash(ctx, "no-such-hash")
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("получили %v, ждали ErrNotFound", err)
	}
}

func TestRepository_Revoke_MarksRevoked(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	tx := newTx(t, pool)
	repo := NewRepository(tx)

	userID := insertUser(t, tx)
	hash := createToken(t, repo, userID)

	rec, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}

	if err := repo.Revoke(ctx, rec.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	after, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash после Revoke: %v", err)
	}
	if after.RevokedAt == nil {
		t.Fatalf("токен должен быть отозван (revoked_at не пуст)")
	}
}

// TestRepository_Revoke_IdempotentKeepsFirstTimestamp проверяет, что повторный
// отзыв НЕ перезаписывает момент отзыва: в SQL стоит COALESCE(revoked_at, now()).
//
// ПОДВОХ тот же, что и в тестах триггера: now() внутри транзакции не меняется,
// поэтому два вызова Revoke подряд дали бы одинаковые времена даже БЕЗ COALESCE.
// Значит нужно искусственно "состарить" revoked_at и проверить, что он уцелел.
func TestRepository_Revoke_IdempotentKeepsFirstTimestamp(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	tx := newTx(t, pool)
	repo := NewRepository(tx)

	userID := insertUser(t, tx)
	hash := createToken(t, repo, userID)

	rec, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}

	// Первый отзыв.
	if err := repo.Revoke(ctx, rec.ID); err != nil {
		t.Fatalf("Revoke #1: %v", err)
	}

	// Искусственно состариваем revoked_at, чтобы результат был отличим от "сейчас".
	if _, err := tx.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() - interval '1 hour' WHERE id = $1`, rec.ID); err != nil {
		t.Fatalf("backdate revoked_at: %v", err)
	}

	// Повторный отзыв — COALESCE обязан сохранить уже имеющуюся дату.
	if err := repo.Revoke(ctx, rec.ID); err != nil {
		t.Fatalf("Revoke #2: %v", err)
	}

	after, err := repo.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if after.RevokedAt == nil {
		t.Fatalf("токен должен быть отозван")
	}
	// Если бы COALESCE не было, revoked_at перезаписался бы на "сейчас".
	// Состаренная дата (≈ час назад) должна остаться — значит она МЕНЬШЕ 30 мин назад.
	if !after.RevokedAt.Before(time.Now().Add(-30 * time.Minute)) {
		t.Errorf("COALESCE не сохранил первый момент отзыва: revoked_at = %v", after.RevokedAt)
	}
}

func TestRepository_RevokeAllForUser(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	tx := newTx(t, pool)
	repo := NewRepository(tx)

	userA := insertUser(t, tx)
	userB := insertUser(t, tx)

	hashA1 := createToken(t, repo, userA)
	hashA2 := createToken(t, repo, userA)
	hashB := createToken(t, repo, userB)

	// Отзываем ВСЕ токены пользователя A.
	if err := repo.RevokeAllForUser(ctx, userA); err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}

	// Оба токена A — отозваны.
	ra1, _ := repo.GetByHash(ctx, hashA1)
	ra2, _ := repo.GetByHash(ctx, hashA2)
	rb, _ := repo.GetByHash(ctx, hashB)

	if ra1.RevokedAt == nil || ra2.RevokedAt == nil {
		t.Errorf("все активные токены A должны быть отозваны")
	}
	// Токен B — не тронут.
	if rb.RevokedAt != nil {
		t.Errorf("токены другого пользователя (B) не должны затрагиваться")
	}
}

// =====================================================================
// WithTx — атомарность на НАСТОЯЩЕМ Postgres. Это главные тесты
// транзакционной ротации: юнит-фейк откат эмулировать не умеет.
//
// Подготовка здесь живёт через ПУЛ (autocommit), потому что данные должны
// пережить транзакции, открываемые самим WithTx. Чистка — явным удалением
// пользователя в t.Cleanup (каскад снесёт и токены).
// =====================================================================

// setupWithTxFixture: пользователь + "старый" токен, закоммиченные в БД.
// Возвращает их и регистрирует очистку.
func setupWithTxFixture(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()

	userID := insertUser(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	hash := "hash-" + uuid.NewString()
	repo := NewRepository(pool)
	if err := repo.Create(ctx, userID, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return userID, hash
}

// TestRepository_WithTx_CommitsOnSuccess — happy path: fn из двух шагов
// (Revoke старого + Create нового — ровно последовательность Rotate)
// закоммитился, и ПОСЛЕ коммита видны оба эффекта разом.
func TestRepository_WithTx_CommitsOnSuccess(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(pool) // пул, НЕ транзакция: WithTx сам откроет свою

	userID, hashOld := setupWithTxFixture(t, pool)

	const hashNew = "hash-new-rotation"
	err := repo.WithTx(ctx, func(store sessionStore) error {
		rec, err := store.GetByHashForUpdate(ctx, hashOld)
		if err != nil {
			return err
		}
		if err := store.Revoke(ctx, rec.ID); err != nil {
			return err
		}
		return store.Create(ctx, userID, hashNew, time.Now().Add(time.Hour))
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	// Оба эффекта НАСТУПИЛИ вместе — как и должно быть после коммита.
	recOld, err := repo.GetByHash(ctx, hashOld)
	if err != nil {
		t.Fatalf("get old after commit: %v", err)
	}
	if recOld.RevokedAt == nil {
		t.Error("старый токен не отозван после коммита")
	}
	if _, err := repo.GetByHash(ctx, hashNew); err != nil {
		t.Errorf("новый токен не сохранился после коммита: %v", err)
	}
}

// TestRepository_WithTx_RollbackOnError — ГЛАВНЫЙ тест всей затеи:
// сбой ПОСЛЕ Revoke, ДО Create → откат возвращает "старый токен жив".
// Именно здесь раньше терялась сессия клиента навсегда.
func TestRepository_WithTx_RollbackOnError(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(pool)

	_, hashOld := setupWithTxFixture(t, pool)

	boom := errors.New("сбой посередине ротации")
	err := repo.WithTx(ctx, func(store sessionStore) error {
		rec, err := store.GetByHashForUpdate(ctx, hashOld)
		if err != nil {
			return err
		}
		if err := store.Revoke(ctx, rec.ID); err != nil {
			return err
		}
		return boom // "упало" МЕЖДУ Revoke и Create
	})
	if !errors.Is(err, boom) {
		t.Fatalf("получили %v, ждали %v", err, boom)
	}

	// КЛЮЧЕВАЯ проверка: Revoke ОТКАТИЛСЯ. Старый токен всё ещё живой —
	// клиент повторит запрос, и ротация пройдёт заново.
	rec, err := repo.GetByHash(ctx, hashOld)
	if err != nil {
		t.Fatalf("get old after rollback: %v", err)
	}
	if rec.RevokedAt != nil {
		t.Errorf("Revoke не откатился: revoked_at = %v", rec.RevokedAt)
	}
}

// TestRepository_WithTx_PanicRollsBack — паника внутри fn тоже обязана
// откатить транзакцию: defer с Rollback в WithTx выполняется при размотке
// стека. Плюс после отката соединение остаётся рабочим.
func TestRepository_WithTx_PanicRollsBack(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(pool)

	_, hashOld := setupWithTxFixture(t, pool)

	// Глушим собственную панику: мы её и устроили. defer выполнится
	// при размотке — этого достаточно, чтобы проверить откат.
	func() {
		defer func() { _ = recover() }()
		_ = repo.WithTx(ctx, func(store sessionStore) error {
			rec, err := store.GetByHashForUpdate(ctx, hashOld)
			if err != nil {
				return err
			}
			if err := store.Revoke(ctx, rec.ID); err != nil {
				return err
			}
			panic("boom внутри транзакции")
		})
	}()

	rec, err := repo.GetByHash(ctx, hashOld)
	if err != nil {
		t.Fatalf("get old after panic: %v", err)
	}
	if rec.RevokedAt != nil {
		t.Errorf("Revoke не откатился после паники: revoked_at = %v", rec.RevokedAt)
	}
}

// TestRepository_TrimToLimit_KeepsNewestActive проверяет SQL трима на
// настоящем Postgres: создаем keep+2 токенов (плюс один СТАРЫЙ ОТЗЫВАННЫЙ,
// чтобы проверить, что отозванные не занимают квоту), тримаем — и убеждаемся:
//   - живых осталось ровно keep, и это САМЫЕ СВЕЖИЕ (последние созданные);
//   - отозванный заранее токен не был "спасён" тримом и не съел квоту.
//
// Токены создаём ЧЕРЕЗ ПУЛ (закоммичены), трим — на транзакции с откатом:
// после теста в БД не остаётся мусора (юзер удалится в Cleanup каскадом).
func TestRepository_TrimToLimit_KeepsNewestActive(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()

	userID := insertUser(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID) })

	repo := NewRepository(pool)
	const keep = 3

	// 1) Заранее отозванный старый токен: не должен попасть в "keep свежих".
	revokedHash := createToken(t, repo, userID)
	rec, err := repo.GetByHash(ctx, revokedHash)
	if err != nil {
		t.Fatalf("get revoked: %v", err)
	}
	if err := repo.Revoke(ctx, rec.ID); err != nil {
		t.Fatalf("revoke old: %v", err)
	}

	// 2) keep+2 живых токенов. После трима выжить должны два ПОСЛЕДНИХ
	//    (fresh2, fresh1) и один перед ними — то есть fresh-хвост.
	const extra = 2
	hashes := make([]string, 0, keep+extra)
	for i := 0; i < keep+extra; i++ {
		hashes = append(hashes, createToken(t, repo, userID))
	}

	// 3) Трим внутри транзакции (rollback в конце — чистота).
	tm := newTx(t, pool)
	txRepo := NewRepository(tm)
	if err := txRepo.TrimToLimit(ctx, userID, keep); err != nil {
		t.Fatalf("TrimToLimit: %v", err)
	}

	// 4) Проверяем по каждому хешу: живой или отозван.
	//    ВАЖНО: читаем ЧЕРЕЗ ТУ ЖЕ транзакцию (txRepo), а не через пул —
	//    UPDATE ещё не закоммичен, и пул видит состояние ДО трима.
	//    Свежие keep штук (конец slices) — живы; всё старше — отозвано.
	for i, h := range hashes {
		rec, err := txRepo.GetByHash(ctx, h)
		if err != nil {
			t.Fatalf("get token #%d: %v", i, err)
		}
		isNewest := i >= extra // индексы extra..end — самые свежие keep штук
		if isNewest && rec.RevokedAt != nil {
			t.Errorf("свежий токен #%d неоправданно отозван", i)
		}
		if !isNewest && rec.RevokedAt == nil {
			t.Errorf("старый токен #%d пережил трим (лимит не сработал)", i)
		}
	}

	// 5) Заранее отозванный остался отозванным (трим его не "реанимировал").
	rec, err = txRepo.GetByHash(ctx, revokedHash)
	if err != nil {
		t.Fatalf("get revoked after trim: %v", err)
	}
	if rec.RevokedAt == nil {
		t.Error("заранее отозванный токен стал живым после трима")
	}
}

// TestRepository_TrimToLimit_UnderLimit_NoOp — если живых сессий МЕНЬШЕ
// лимита, трим ничего не делает (и тем более не роняет запрос).
func TestRepository_TrimToLimit_UnderLimit_NoOp(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	tx := newTx(t, pool)
	repo := NewRepository(tx)

	userID := insertUser(t, tx)
	h1 := createToken(t, repo, userID)
	h2 := createToken(t, repo, userID)

	if err := repo.TrimToLimit(ctx, userID, 10); err != nil {
		t.Fatalf("TrimToLimit: %v", err)
	}

	// Оба на месте и живы.
	for _, h := range []string{h1, h2} {
		rec, err := repo.GetByHash(ctx, h)
		if err != nil {
			t.Fatalf("get %q: %v", h, err)
		}
		if rec.RevokedAt != nil {
			t.Errorf("токен %q отозван, хотя сессий меньше лимита", h)
		}
	}
}

// TestRepository_DeleteExpired_DeletesExpiredKeepsLive — правило чистки:
// убираем ТОЛЬКО expires_at < now, а живые (и отозванные, но не протухшие —
// на них держится детект кражи) остаются.
func TestRepository_DeleteExpired_DeletesExpiredKeepsLive(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(pool)

	userID := insertUser(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID) })

	// Токен, протухший час назад.
	const expiredHash = "hash-expired-cleanup"
	if err := repo.Create(ctx, userID, expiredHash, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	// Токен, живой ещё час.
	const liveHash = "hash-live-cleanup"
	if err := repo.Create(ctx, userID, liveHash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create live: %v", err)
	}

	// Заметь: "протухший" создаём прямо с истёкшим expires_at — колонка
	// валидности не имеет CHECK, и это ровно состояние после простоя.
	deleted, err := repo.DeleteExpired(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	// Удалён "хотя бы наш" (в dev-БД могут быть и чужие протухшие).
	if deleted < 1 {
		t.Errorf("DeleteExpired удалил %d, ждали >= 1", deleted)
	}

	// Протухший исчез.
	if _, err := repo.GetByHash(ctx, expiredHash); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("протухший токен не удалён: %v", err)
	}
	// Живой остался — и не тронут.
	rec, err := repo.GetByHash(ctx, liveHash)
	if err != nil {
		t.Fatalf("живой токен пропал: %v", err)
	}
	if rec.RevokedAt != nil {
		t.Errorf("живой токен был отозван чисткой: %v", rec.RevokedAt)
	}
}
