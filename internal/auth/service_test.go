package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/timqa/my-rest-api/internal/apperr"
)

// =====================================================================
// Фейки: подменяем реальные зависимости "умными заглушками".
//
// fakeSessionStore реализует интерфейс sessionStore (см. service.go).
// Он и прикидывается хранилищем сессий, и ЗАПОМИНАЕТ, что у него звали, —
// чтобы в тестах можно было проверить "а отозвали ли сессии?".
// =====================================================================

type fakeSessionStore struct {
	// Управляемые ответы: что вернуть по вызову.
	getByHashRecord refreshRecord
	getByHashErr    error
	createErr       error
	revokeErr       error
	revokeAllErr    error

	// Записанные факты вызовов: что и с чем позвали.
	revokedIDs       []uuid.UUID // какие id пришли в Revoke
	revokeAllUserIDs []uuid.UUID // каких юзеров отозвали целиком
	created          []refreshRecord

	// DeleteExpired
	deleteExpiredErr   error
	deleteExpiredCount int64
	deleteExpiredNow   time.Time // с каким "сейчас" позвали
}

func (f *fakeSessionStore) Create(_ context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, refreshRecord{UserID: userID, TokenHash: tokenHash, ExpiresAt: expiresAt})
	return nil
}

func (f *fakeSessionStore) GetByHash(_ context.Context, _ string) (refreshRecord, error) {
	return f.getByHashRecord, f.getByHashErr
}

// GetByHashForUpdate — для фейка не отличается от GetByHash: блокировка
// строки — механика НАСТОЯЩЕЙ БД, у фейка блокировать нечего. Сервисная
// логика поверх (проверки revoked_at/expires_at) проверяется и так.
func (f *fakeSessionStore) GetByHashForUpdate(_ context.Context, _ string) (refreshRecord, error) {
	return f.getByHashRecord, f.getByHashErr
}

// WithTx — фейк не умеет транзакций, поэтому просто выполняет fn на себе.
// Это ОСОЗНАННОЕ упрощение: фейк проверяет бизнес-логику (какие вызовы
// и в каком порядке), а реальную атомарность — откат/коммит на настоящем
// Postgres — проверяют интеграционные тесты WithTx (repository_test.go).
// Следствие: состояние, изменённое fn перед ошибкой, у фейка останется —
// в юнит-тестах это не проверяется.
func (f *fakeSessionStore) WithTx(_ context.Context, fn func(sessionStore) error) error {
	return fn(f)
}

func (f *fakeSessionStore) Revoke(_ context.Context, id uuid.UUID) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revokedIDs = append(f.revokedIDs, id)
	return nil
}

func (f *fakeSessionStore) RevokeAllForUser(_ context.Context, userID uuid.UUID) error {
	if f.revokeAllErr != nil {
		return f.revokeAllErr
	}
	f.revokeAllUserIDs = append(f.revokeAllUserIDs, userID)
	return nil
}

// DeleteExpired — fake возвращает заранее заданный счётчик и запоминает,
// с каким "сейчас" его звали (в тесте проверим, что сервис передаёт время).
func (f *fakeSessionStore) DeleteExpired(_ context.Context, now time.Time) (int64, error) {
	f.deleteExpiredNow = now
	return f.deleteExpiredCount, f.deleteExpiredErr
}

// fakeRoleLookup реализует auth.RoleLookup — "узнать роль по id".
type fakeRoleLookup struct {
	role string
	err  error
}

func (f *fakeRoleLookup) RoleByID(_ context.Context, _ uuid.UUID) (string, error) {
	return f.role, f.err
}

// newTestService собирает сервис с настоящим TokenManager (нам нужны реальные
// токены), но с ФЕЙКОВЫМ хранилищем и фейковым RoleLookup.
func newTestService(store sessionStore, roles RoleLookup) *Service {
	tm := NewTokenManager("test-secret", time.Hour)
	return NewService(tm, store, roles, time.Hour)
}

// =====================================================================
// Rotate — "счастливый путь": живой токен меняется на новую пару.
// =====================================================================

func TestService_Rotate_Success(t *testing.T) {
	userID := uuid.New()
	recID := uuid.New()

	store := &fakeSessionStore{
		getByHashRecord: refreshRecord{
			ID:        recID,
			UserID:    userID,
			ExpiresAt: time.Now().Add(time.Hour), // живой
			RevokedAt: nil,                       // ещё не отозван
		},
	}
	svc := newTestService(store, &fakeRoleLookup{role: "admin"})

	pair, err := svc.Rotate(context.Background(), "some-refresh-token")
	if err != nil {
		t.Fatalf("Rotate вернул ошибку: %v", err)
	}

	// Вернулась полноценная пара токенов.
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("вернулась пустая пара: %+v", pair)
	}

	// Старый токен ДОЛЖЕН быть отозван (ротация!).
	if len(store.revokedIDs) != 1 || store.revokedIDs[0] != recID {
		t.Errorf("ожидали отзыв старого токена %v, получили %v", recID, store.revokedIDs)
	}

	// Новая сессия ДОЛЖНА быть сохранена.
	if len(store.created) != 1 {
		t.Errorf("ожидали сохранение новой сессии, получили %d", len(store.created))
	}
}

// =====================================================================
// Rotate — ДЕТЕКТ КРАЖИ: пришёл уже отозванный токен.
// Это самый важный кейс: переиспользование старого токена = утечка,
// поэтому должны умереть ВСЕ сессии пользователя.
// =====================================================================

func TestService_Rotate_RevokedToken_RevokesAllSessions(t *testing.T) {
	userID := uuid.New()
	revokedAt := time.Now().Add(-time.Minute) // когда-то был отозван

	store := &fakeSessionStore{
		getByHashRecord: refreshRecord{
			ID:        uuid.New(),
			UserID:    userID,
			ExpiresAt: time.Now().Add(time.Hour),
			RevokedAt: &revokedAt, // уже отозван → это повторное использование
		},
	}
	svc := newTestService(store, &fakeRoleLookup{role: "user"})

	_, err := svc.Rotate(context.Background(), "stolen-token")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("получили %v, ждали ErrInvalidToken", err)
	}

	// КЛЮЧЕВАЯ проверка: должны были отозвать ВСЕ сессии этого пользователя.
	if len(store.revokeAllUserIDs) != 1 || store.revokeAllUserIDs[0] != userID {
		t.Errorf("ожидали RevokeAllForUser(%v), получили %v", userID, store.revokeAllUserIDs)
	}
}

// =====================================================================
// Rotate — прочие "плохие" входы. Все обязаны дать ErrInvalidToken,
// но БЕЗ побочного эффекта "отозвать всё" (это только для кражи).
// Табличный тест: одна проверка — разные сценарии.
// =====================================================================

func TestService_Rotate_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name  string
		store *fakeSessionStore
		roles *fakeRoleLookup
	}{
		{
			name:  "токен неизвестен",
			store: &fakeSessionStore{getByHashErr: apperr.ErrNotFound},
			roles: &fakeRoleLookup{role: "user"},
		},
		{
			name: "токен просрочен",
			store: &fakeSessionStore{
				getByHashRecord: refreshRecord{
					UserID:    uuid.New(),
					ExpiresAt: time.Now().Add(-time.Hour), // протух
				},
			},
			roles: &fakeRoleLookup{role: "user"},
		},
		{
			name: "пользователь удалён",
			store: &fakeSessionStore{
				getByHashRecord: refreshRecord{
					UserID:    uuid.New(),
					ExpiresAt: time.Now().Add(time.Hour),
				},
			},
			roles: &fakeRoleLookup{err: apperr.ErrNotFound}, // роль не достать
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(tt.store, tt.roles)

			_, err := svc.Rotate(context.Background(), "token")
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("получили %v, ждали ErrInvalidToken", err)
			}

			// "Отозвать всё" допустимо ТОЛЬКО при детекте кражи.
			if len(tt.store.revokeAllUserIDs) != 0 {
				t.Errorf("неожиданный RevokeAllForUser: %v", tt.store.revokeAllUserIDs)
			}
		})
	}
}

// =====================================================================
// Revoke (logout) — идемпотентность: неизвестный токен НЕ ошибка.
// =====================================================================

func TestService_Revoke_UnknownToken_NoError(t *testing.T) {
	store := &fakeSessionStore{getByHashErr: apperr.ErrNotFound}
	svc := newTestService(store, &fakeRoleLookup{role: "user"})

	if err := svc.Revoke(context.Background(), "ghost-token"); err != nil {
		t.Fatalf("Revoke не должен падать на неизвестном токене, получили: %v", err)
	}
}

// =====================================================================
// Rotate — сбой сохранения новой пары ПОСЛЕ отзыва старого токена.
//
// Это тот кейс, ради которого вводилась транзакция: раньше здесь
// клиент терял сессию НАВСЕГДА (старый токен уже отозван, новый не
// сохранён). Теперь ошибка должна честно прилететь наверх, а на
// настоящей БД — с откатом Revoke (см. интеграционный тест
// TestRepository_WithTx_RollbackOnError).
// =====================================================================

func TestService_Rotate_CreateFails_PropagatesError(t *testing.T) {
	createErr := errors.New("db down mid-rotate")
	store := &fakeSessionStore{
		getByHashRecord: refreshRecord{
			ID:        uuid.New(),
			UserID:    uuid.New(),
			ExpiresAt: time.Now().Add(time.Hour), // живой, не отозван
		},
		createErr: createErr,
	}
	svc := newTestService(store, &fakeRoleLookup{role: "user"})

	_, err := svc.Rotate(context.Background(), "valid-token")
	if !errors.Is(err, createErr) {
		t.Fatalf("получили %v, ждали ошибку сохранения %v", err, createErr)
	}
}

// =====================================================================
// CleanupExpired — один проход фоновой чистки (цикл RunCleanup не
// юнит-тестируется: он про тайминги, а тайминги в тестах не проверяют).
// =====================================================================

func TestService_CleanupExpired_ReturnsCount(t *testing.T) {
	store := &fakeSessionStore{deleteExpiredCount: 3}
	svc := newTestService(store, &fakeRoleLookup{role: "user"})

	n, err := svc.CleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("CleanupExpired: %v", err)
	}
	if n != 3 {
		t.Errorf("вернуло %d, ждали 3", n)
	}
	// Сервис передаёт в репозиторий "сейчас" — не нулевое время.
	if store.deleteExpiredNow.IsZero() {
		t.Error("в репозиторий не передано время now")
	}
}

func TestService_CleanupExpired_PropagatesError(t *testing.T) {
	store := &fakeSessionStore{deleteExpiredErr: errors.New("db down")}
	svc := newTestService(store, &fakeRoleLookup{role: "user"})

	if _, err := svc.CleanupExpired(context.Background()); !errors.Is(err, store.deleteExpiredErr) {
		t.Fatalf("получили %v, ждали %v", err, store.deleteExpiredErr)
	}
}
