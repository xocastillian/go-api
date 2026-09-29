//go:build integration

// Интеграционные тесты репозитория user: ходят в НАСТОЯЩУЮ Postgres.
// Запуск: go test -tags integration ./internal/user/
//
// Каждый тест открывает транзакцию и в конце ОТКАТЫВАЕТ её (newTx),
// поэтому данные тестов не оседают в БД.
package user

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timqa/my-rest-api/internal/apperr"
	"github.com/timqa/my-rest-api/internal/testsupport"
)

// newTx открывает транзакцию и регистрирует её откат в t.Cleanup.
// Возвращает pgx.Tx, который подходит и в NewRepository (там ждёт postgres.DBTX).
func newTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()

	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// Откат по завершении теста — гарантированно чистит всё, что мы наделали.
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// uniqueEmail даёт email, не конфликтующий с dev-данными в общей БД.
func uniqueEmail() string {
	return "user-" + uuid.NewString() + "@test.local"
}

func TestRepository_CreateThenGet(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(newTx(t, pool))

	created, err := repo.Create(ctx, User{Email: uniqueEmail(), PasswordHash: "hash", Role: RoleUser})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// id и timestamps генерирует БД — проверим, что они реально пришли.
	if created.ID == uuid.Nil {
		t.Errorf("БД не вернула id")
	}
	if created.CreatedAt.IsZero() {
		t.Errorf("БД не вернула created_at")
	}

	byEmail, err := repo.GetByEmail(ctx, created.Email)
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if byEmail.ID != created.ID {
		t.Errorf("GetByEmail вернул не того: %v != %v", byEmail.ID, created.ID)
	}

	byID, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if byID.Email != created.Email {
		t.Errorf("GetByID email: получили %q, ждали %q", byID.Email, created.Email)
	}
}

func TestRepository_Create_DuplicateEmail(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(newTx(t, pool))

	email := uniqueEmail()
	if _, err := repo.Create(ctx, User{Email: email, PasswordHash: "h", Role: RoleUser}); err != nil {
		t.Fatalf("первый Create: %v", err)
	}

	// Второй раз тот же email — Postgres вернёт 23505, репозиторий должен
	// перевести это в доменную ErrEmailTaken.
	_, err := repo.Create(ctx, User{Email: email, PasswordHash: "h", Role: RoleUser})
	if !errors.Is(err, apperr.ErrEmailTaken) {
		t.Fatalf("получили %v, ждали ErrEmailTaken", err)
	}
}

func TestRepository_GetByID_NotFound(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(newTx(t, pool))

	_, err := repo.GetByID(ctx, uuid.New())
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("получили %v, ждали ErrNotFound", err)
	}
}

func TestRepository_GetByEmail_NotFound(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(newTx(t, pool))

	_, err := repo.GetByEmail(ctx, uniqueEmail())
	if !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("получили %v, ждали ErrNotFound", err)
	}
}

// TestRepository_UpdatePassword_BumpsUpdatedAtViaTrigger проверяет сразу две вещи:
// что хеш действительно записался И что триггер users_set_updated_at сработал.
func TestRepository_UpdatePassword_BumpsUpdatedAtViaTrigger(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	tx := newTx(t, pool)
	repo := NewRepository(tx)

	created, err := repo.Create(ctx, User{Email: uniqueEmail(), PasswordHash: "old", Role: RoleUser})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// ПОДВОХ: триггер ставит updated_at = now(), а now() внутри ОДНОЙ транзакции
	// не меняется (это transaction_timestamp). Без трюка ниже created_at и
	// updated_at совпали бы, и мы не увидели бы работу триггера.
	//
	// Поэтому искусственно "состарим" updated_at: временно выключим триггер,
	// поставим заведомо старое значение, включим триггер обратно.
	// Всё это внутри транзакции — нарушения в БД не останутся.
	if _, err := tx.Exec(ctx, `ALTER TABLE users DISABLE TRIGGER users_set_updated_at`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET updated_at = now() - interval '1 day' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE users ENABLE TRIGGER users_set_updated_at`); err != nil {
		t.Fatalf("enable trigger: %v", err)
	}

	backdated, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID после backdate: %v", err)
	}

	// Меняем пароль — на этом UPDATE триггер обязан выставить updated_at = now().
	if err := repo.UpdatePassword(ctx, created.ID, "new"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	got, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PasswordHash != "new" {
		t.Errorf("хеш не обновился: получили %q", got.PasswordHash)
	}
	if !got.UpdatedAt.After(backdated.UpdatedAt) {
		t.Errorf("триггер не сдвинул updated_at: было %v, стало %v", backdated.UpdatedAt, got.UpdatedAt)
	}
}

func TestRepository_Delete_NotFound(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(newTx(t, pool))

	if err := repo.Delete(ctx, uuid.New()); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("получили %v, ждали ErrNotFound", err)
	}
}

// TestRepository_Delete_Cascades — ради этого теста во многом и делался
// рефакторинг: проверяем настоящий ON DELETE CASCADE на уровне схемы.
func TestRepository_Delete_Cascades(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	tx := newTx(t, pool)
	repo := NewRepository(tx)

	created, err := repo.Create(ctx, User{Email: uniqueEmail(), PasswordHash: "h", Role: RoleUser})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Заводим по одной дочерней записи КАЖДОГО вида.
	if _, err := tx.Exec(ctx,
		`INSERT INTO notes (user_id, title) VALUES ($1, $2)`, created.ID, "note"); err != nil {
		t.Fatalf("insert note: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '1 day')`,
		created.ID, uuid.NewString()); err != nil {
		t.Fatalf("insert refresh token: %v", err)
	}

	// Удаляем пользователя ОДНОЙ командой.
	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Сам пользователь исчез.
	if _, err := repo.GetByID(ctx, created.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("пользователь не удалён: %v", err)
	}

	// Каскад обязан снести дочерние записи.
	var notes, tokens int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM notes WHERE user_id = $1`, created.ID).Scan(&notes); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, created.ID).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if notes != 0 {
		t.Errorf("заметки не удалены каскадом: осталось %d", notes)
	}
	if tokens != 0 {
		t.Errorf("refresh-токены не удалены каскадом: осталось %d", tokens)
	}
}

// TestRepository_List_Pagination — в БД есть чужие (dev) данные, поэтому
// проверяем не "ровно N", а саму механику limit/offset и стабильность порядка.
func TestRepository_List_Pagination(t *testing.T) {
	pool := testsupport.NewPool(t)
	ctx := context.Background()
	repo := NewRepository(newTx(t, pool))

	for i := 0; i < 3; i++ {
		if _, err := repo.Create(ctx, User{Email: uniqueEmail(), PasswordHash: "h", Role: RoleUser}); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}

	full, err := repo.List(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(full) < 3 {
		t.Fatalf("List вернул %d, ждали минимум 3 (наши)", len(full))
	}

	// limit реально ограничивает размер страницы.
	page1, err := repo.List(ctx, 2, 0)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("limit не сработал: вернулось %d, ждали 2", len(page1))
	}

	// offset сдвигает выборку — первая страница смещается.
	page2, err := repo.List(ctx, 2, 2)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2) >= 1 && len(page1) >= 1 && page1[0].ID == page2[0].ID {
		t.Errorf("offset не сдвинул выборку: первые id совпали (%v)", page1[0].ID)
	}
}
