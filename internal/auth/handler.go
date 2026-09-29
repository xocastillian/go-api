package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/httputil"
)

// Handler — HTTP-ручки жизненного цикла сессии: обновление и отзыв.
// Логин/регистрация живут в фиче user (там проверяется пароль),
// но токены для них выпускает тот же auth.Service.
// Роуты — в routes.go, DTO — в dto.go (как и в фиче user).
type Handler struct {
	sessions *Service

	// tokens нужен для AuthRequired на ЗАЩИЩЁННЫХ ручках (logout-all).
	// Это СВОЙ пакетный тип, не чужой — та же связка "выпуск/проверка",
	// что и у user.Handler, но без импорта другой фичи.
	tokens *TokenManager
}

// NewHandler — конструктор, зависимости приходят снаружи (DI).
// Сигнатура расширилась (был только sessions): защищённым ручкам нужен
// TokenManager, composition root (main.go) передаёт его той же строкой.
func NewHandler(sessions *Service, tokens *TokenManager) *Handler {
	return &Handler{sessions: sessions, tokens: tokens}
}

// Refresh — POST /auth/refresh
// Ручка НЕ защищена AuthRequired: её зовут как раз тогда, когда
// access-токен протух. От "незваных гостей" защищает сам refresh-токен.
//
// @Summary      Обновление пары токенов
// @Description  Принимает refresh-токен, отзывает его и выдаёт новую пару (ротация).
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        request body refreshRequest true "Refresh-токен"
// @Success      200 {object} tokenPairResponse
// @Failure      400 {object} httputil.ErrorResponse "Невалидное тело запроса"
// @Failure      401 {object} httputil.ErrorResponse "Недействительный refresh-токен"
// @Router       /auth/refresh [post]
func (h *Handler) Refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, httputil.ErrorResponse{Error: err.Error()})
		return
	}

	pair, err := h.sessions.Rotate(c.Request.Context(), req.RefreshToken)
	if err != nil {
		// ErrInvalidToken живёт в пакете auth, а не в общем apperr,
		// поэтому маппим его в 401 явно...
		if errors.Is(err, ErrInvalidToken) {
			c.JSON(http.StatusUnauthorized, httputil.ErrorResponse{Error: "invalid refresh token"})
			return
		}
		// ...а остальное (например, сбой БД) отдаём общему хелперу:
		// он залогирует детали и вернёт 500, не раскрывая их клиенту.
		httputil.WriteError(c, err)
		return
	}

	c.JSON(http.StatusOK, toTokenPairResponse(pair))
}

// Logout — POST /auth/logout
// Отзывает refresh-токен. Идемпотентно: повторный вызов — тоже 204.
//
// @Summary      Выход (отзыв refresh-токена)
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        request body refreshRequest true "Refresh-токен"
// @Success      204 "Сессия отозвана"
// @Failure      400 {object} httputil.ErrorResponse "Невалидное тело запроса"
// @Router       /auth/logout [post]
func (h *Handler) Logout(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, httputil.ErrorResponse{Error: err.Error()})
		return
	}

	if err := h.sessions.Revoke(c.Request.Context(), req.RefreshToken); err != nil {
		httputil.WriteError(c, err)
		return
	}

	// 204 No Content: операция прошла, телу ответа нечего показывать.
	c.Status(http.StatusNoContent)
}

// LogoutAll — POST /auth/logout-all (защищённая ручка).
// В отличие от /logout, требует валидный access-токен: «все МОИ сессии»
// определяется личностью из токена, а не содержимым тела.
//
// @Summary      Выход со всех устройств
// @Description  Отзывает ВСЕ активные refresh-сессии пользователя, включая текущую.
// @Tags         auth
// @Security     BearerAuth
// @Success      204 "Все сессии отозваны"
// @Failure      401 {object} httputil.ErrorResponse "Нет или протух токен"
// @Router       /auth/logout-all [post]
func (h *Handler) LogoutAll(c *gin.Context) {
	// Кого «выкидывать» — берём из токена. Клиент НЕ передаёт id в теле:
	// иначе подсунув чужой id, можно было бы разлогинить любого. Тело
	// запроса не читаем вовсе: ручке нечего в нём решать.
	id := UserID(c)

	// Вся логика — в сервисе (он же у ChangePassword и детекта кражи):
	// метка revoked_at на ВСЕ живые refresh-токены юзера. Метод
	// идемпотентен: нет активных сессий — тоже успех, а не ошибка
	// (сервис возвращает ошибку только при сбое БД → 500 через WriteError).
	//
	// ВАЖНО: сессия ЭТОГО устройства тоже умирает — пользователь, нажимая
	// «выйти со всех устройств», ожидает выйти и здесь.
	// Привычный trade-off со stateless access-токеном остаётся: сам
	// access-токен доживёт свои ~15 минут (см. примечание в ChangePassword
	// фичи user), но обновиться по любому refresh-токену уже не выйдет.
	if err := h.sessions.RevokeAllForUser(c.Request.Context(), id); err != nil {
		httputil.WriteError(c, err)
		return
	}

	// 204 No Content: тело не нужно. (В т.ч. и потому, что оно могло бы
	// намекнуть «сколько сессий убито» — не отдаём.)
	c.Status(http.StatusNoContent)
}
