// Package httputil — общие помощники HTTP-слоя, не привязанные к конкретной фиче.
package httputil

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/apperr"
)

// ErrorResponse — единый формат ошибки во всех ручках: {"error": "..."}.
// Тип реально используется в ответах (а не только в swagger-аннотациях):
// так спецификация не может разойтись с кодом.
type ErrorResponse struct {
	Error string `json:"error"`
}

// WriteError переводит доменную ошибку в HTTP-статус и JSON-ответ.
// Общий хелпер для всех фич (user, note, ...): маппинг ошибок живёт
// в одном месте, а не дублируется в каждом хендлере.
func WriteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, apperr.ErrEmailTaken):
		c.JSON(http.StatusConflict, ErrorResponse{Error: "email already taken"})
	case errors.Is(err, apperr.ErrInvalidCredentials):
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid credentials"})
	case errors.Is(err, apperr.ErrNotFound):
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "not found"})
	default:
		// Внутренние ошибки НЕ отдаём клиенту (не палим детали) —
		// логируем на сервере, а клиенту отвечаем обобщённо.
		slog.Error("internal error", "err", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
	}
}
