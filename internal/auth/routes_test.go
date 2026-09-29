package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// =====================================================================
// Тесты роутов фичи auth: проверяем СКЛЕЙКУ (роут + middleware + хендлер),
// а не бизнес-логику (её покрывают service_test.go и repository_test.go).
//
// Фейки берём готовые из service_test.go — они в том же пакете.
// =====================================================================

// newAuthRouter собирает МИНИ-приложение с настоящим auth.Handler:
// фейковое хранилище сессий, но РЕАЛЬНЫЙ TokenManager и РЕАЛЬНЫЙ роутинг —
// ровно та склейка, что делает main.go, только без БД.
func newAuthRouter(t *testing.T) (*gin.Engine, *fakeSessionStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := &fakeSessionStore{}
	tm := NewTokenManager("test-secret", time.Hour)
	svc := NewService(tm, store, &fakeRoleLookup{role: "user"}, time.Hour)
	handler := NewHandler(svc, tm)

	r := gin.New()
	handler.RegisterRoutes(r.Group("/api/v1"))
	return r, store
}

// doAuth выполняет запрос с опциональным Authorization-заголовком.
func doAuth(t *testing.T, r *gin.Engine, method, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestLogoutAll_RequiresAuth — ручка ДОЛЖНА быть закрыта AuthRequired.
// Регрессия, от которой защищаемся: однажды роут напишут без middleware,
// и «разлогинить всех» сможет кто угодно, не представившись вовсе.
func TestLogoutAll_RequiresAuth(t *testing.T) {
	r, store := newAuthRouter(t)

	rec := doAuth(t, r, http.MethodPost, "/api/v1/auth/logout-all", "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: получили %d, ждали 401; тело: %s", rec.Code, rec.Body.String())
	}

	// КЛЮЧЕВОЕ: не только 401, но и НОЛЬ побочных эффектов —
	// сессии не должны быть отозваны, раз личность не установлена.
	if len(store.revokeAllUserIDs) != 0 {
		t.Errorf("без токена отозваны сессии: %v", store.revokeAllUserIDs)
	}
}

// TestLogoutAll_RevokesAllSessions — счастливый путь: валидный токен →
// 204, и сервис отозвал все сессии РОВНО этого пользователя.
func TestLogoutAll_RevokesAllSessions(t *testing.T) {
	r, store := newAuthRouter(t)
	tm := NewTokenManager("test-secret", time.Hour)

	// Личность зашиваем в токен — middleware обязана доставить её до хендлера.
	id := uuid.New()
	token, err := tm.Issue(id, "user")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	rec := doAuth(t, r, http.MethodPost, "/api/v1/auth/logout-all", token)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("получили %d, ждали 204; тело: %s", rec.Code, rec.Body.String())
	}

	// Отозваны сессии именно ТОКЕН-владельца: id пришёл из middleware,
	// а не откуда-то ещё (тело запроса пустое).
	if len(store.revokeAllUserIDs) != 1 || store.revokeAllUserIDs[0] != id {
		t.Errorf("RevokeAllForUser: ждали [%v], получили %v", id, store.revokeAllUserIDs)
	}
}

// TestLogoutAll_OtherUsersUntouched — «все сессии» — это только сессии
// владельца токена. Прямой аналог интеграционного теста репозитория,
// но через весь HTTP-слой.
func TestLogoutAll_OtherUsersUntouched(t *testing.T) {
	r, store := newAuthRouter(t)
	tm := NewTokenManager("test-secret", time.Hour)

	other := uuid.New()
	token, err := tm.Issue(other, "user")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if rec := doAuth(t, r, http.MethodPost, "/api/v1/auth/logout-all", token); rec.Code != http.StatusNoContent {
		t.Fatalf("получили %d, ждали 204", rec.Code)
	}

	// В сторе отозван только владелец токена — чужой id не появлялся.
	if len(store.revokeAllUserIDs) != 1 || store.revokeAllUserIDs[0] != other {
		t.Errorf("отозваны сессии чужих пользователей: %v", store.revokeAllUserIDs)
	}
}
