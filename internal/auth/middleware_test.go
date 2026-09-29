package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// setupRouter собирает МИНИ-приложение с нашими middleware — ровно так,
// как это делает реальный код, но без БД и лишних ручек.
//
// Ручка /me закрыта только AuthRequired и «отражает» то, что middleware
// положила в контекст. Ручка /admin закрыта ещё и RequireRole("admin").
func setupRouter(tm *TokenManager) *gin.Engine {
	gin.SetMode(gin.TestMode) // без debug-простыни в выводе тестов

	r := gin.New()

	// Группа, защищённая аутентификацией.
	authed := r.Group("/", AuthRequired(tm))
	authed.GET("/me", func(c *gin.Context) {
		// Если middleware сработала — здесь будут валидные значения.
		c.JSON(http.StatusOK, gin.H{
			"user_id": UserID(c).String(),
			"role":    Role(c),
		})
	})

	// Группа, защищённая ещё и по роли.
	adminOnly := r.Group("/", AuthRequired(tm), RequireRole("admin"))
	adminOnly.GET("/admin", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	return r
}

// do — помощник: выполняет запрос против тестового роутера и возвращает
// записанный ответ. Значительно короче, чем расписывать httptest каждый раз.
func do(r *gin.Engine, method, path, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req) // прогоняем запрос через всё приложение
	return rec
}

// TestAuthRequired_RejectsInvalid — табличный тест: любой «плохой» вход
// обязан дать 401 и НЕ пустить к хендлеру.
func TestAuthRequired_RejectsInvalid(t *testing.T) {
	tm := NewTokenManager("test-secret", time.Hour)
	r := setupRouter(tm)

	// Токен, подписанный ДРУГИМ секретом — должен быть отвергнут.
	otherTM := NewTokenManager("other-secret", time.Hour)
	forged, err := otherTM.Issue(uuid.New(), "user")
	if err != nil {
		t.Fatalf("Issue(forged): %v", err)
	}

	tests := []struct {
		name   string
		header string
	}{
		{"нет заголовка", ""},
		{"не Bearer-схема", "Token abc"},
		{"мусор вместо токена", "Bearer not-a-jwt"},
		{"подпись чужим секретом", "Bearer " + forged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(r, http.MethodGet, "/me", tt.header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("получили %d, ждали 401; тело: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAuthRequired_ValidToken_SetsContext — счастливый путь: валидный токен
// пропускается, а UserID/Role из него оказываются в контексте хендлера.
func TestAuthRequired_ValidToken_SetsContext(t *testing.T) {
	tm := NewTokenManager("test-secret", time.Hour)
	r := setupRouter(tm)

	id := uuid.New()
	token, err := tm.Issue(id, "moderator")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	rec := do(r, http.MethodGet, "/me", "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("получили %d, ждали 200; тело: %s", rec.Code, rec.Body.String())
	}

	// Хендлер вернул то, что достал из контекста. Проверяем, что совпало.
	var body struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if body.UserID != id.String() {
		t.Errorf("user_id: получили %q, ждали %q", body.UserID, id.String())
	}
	if body.Role != "moderator" {
		t.Errorf("role: получили %q, ждали %q", body.Role, "moderator")
	}
}

// TestRequireRole — авторизация по роли: админ проходит, обычный — 403.
// Заметь разницу: 401 = "не представился", 403 = "представился, но нельзя".
func TestRequireRole(t *testing.T) {
	tm := NewTokenManager("test-secret", time.Hour)
	r := setupRouter(tm)

	tests := []struct {
		name     string
		role     string
		wantCode int
	}{
		{"admin проходит", "admin", http.StatusOK},
		{"user получает 403", "user", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := tm.Issue(uuid.New(), tt.role)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			rec := do(r, http.MethodGet, "/admin", "Bearer "+token)
			if rec.Code != tt.wantCode {
				t.Fatalf("получили %d, ждали %d", rec.Code, tt.wantCode)
			}
		})
	}
}
