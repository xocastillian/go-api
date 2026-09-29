package user

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/auth"
	"github.com/timqa/my-rest-api/internal/httputil"
)

// Handler — HTTP-обработчики пользователей.
// DTO лежат в dto.go, роуты — в routes.go.
type Handler struct {
	users *Service

	// sessions ВЫПУСКАЕТ пару токенов (логин/регистрация).
	sessions *auth.Service

	// tokens ПРОВЕРЯЕТ access-токен в запросах (middleware в routes.go).
	// Два разных объекта: один выдаёт сессию, другой проверяет короткий токен.
	tokens *auth.TokenManager
}

// NewHandler — конструктор. Зависимости передаём снаружи (DI).
func NewHandler(users *Service, sessions *auth.Service, tokens *auth.TokenManager) *Handler {
	return &Handler{users: users, sessions: sessions, tokens: tokens}
}

// Register — POST /auth/register
//
// @Summary      Регистрация пользователя
// @Description  Создаёт аккаунт и сразу возвращает пару токенов (access + refresh).
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        request body registerRequest true "Email и пароль"
// @Success      201 {object} authResponse
// @Failure      400 {object} httputil.ErrorResponse "Невалидное тело запроса"
// @Failure      409 {object} httputil.ErrorResponse "Email уже занят"
// @Router       /auth/register [post]
func (h *Handler) Register(c *gin.Context) {
	var req registerRequest

	// ShouldBindJSON: читает тело запроса, парсит JSON в структуру
	// и прогоняет валидацию по тегам binding. Ошибку вернёт сразу.
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, httputil.ErrorResponse{Error: err.Error()})
		return
	}

	// c.Request.Context() — контекст HTTP-запроса. Передаём его в сервис,
	// чтобы БД-запрос отменился, если клиент отвалился.
	u, err := h.users.Register(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		httputil.WriteError(c, err)
		return
	}

	// Сразу логиним: после регистрации клиенту удобно получить токены,
	// чтобы не делать отдельный запрос на /login.
	pair, err := h.sessions.IssuePair(c.Request.Context(), u.ID, string(u.Role))
	if err != nil {
		httputil.WriteError(c, err)
		return
	}

	c.JSON(http.StatusCreated, authResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		User:         toResponse(u),
	})
}

// Login — POST /auth/login
//
// @Summary      Вход
// @Description  Проверяет email/пароль и возвращает пару токенов.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        request body loginRequest true "Email и пароль"
// @Success      200 {object} authResponse
// @Failure      400 {object} httputil.ErrorResponse "Невалидное тело запроса"
// @Failure      401 {object} httputil.ErrorResponse "Неверные учётные данные"
// @Router       /auth/login [post]
func (h *Handler) Login(c *gin.Context) {
	var req loginRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, httputil.ErrorResponse{Error: err.Error()})
		return
	}

	u, err := h.users.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		httputil.WriteError(c, err)
		return
	}

	// Пароль совпал — выпускаем ПАРУ токенов (access + refresh).
	pair, err := h.sessions.IssuePair(c.Request.Context(), u.ID, string(u.Role))
	if err != nil {
		httputil.WriteError(c, err)
		return
	}

	c.JSON(http.StatusOK, authResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		User:         toResponse(u),
	})
}

// Me — GET /me (защищённая ручка).
// До неё запрос доходит только через auth.AuthRequired, иначе — 401
// ещё до вызова этого метода. Поэтому здесь id гарантированно есть.
//
// @Summary      Профиль текущего пользователя
// @Tags         user
// @Produce      json
// @Security     BearerAuth
// @Success      200 {object} userResponse
// @Failure      401 {object} httputil.ErrorResponse "Нет или протух токен"
// @Failure      404 {object} httputil.ErrorResponse "Пользователь не найден"
// @Router       /me [get]
func (h *Handler) Me(c *gin.Context) {
	// Достаём личность, которую middleware положила в контекст.
	// Парсить токен повторно не нужно — такая работа уже сделана.
	id := auth.UserID(c)

	// Свежие данные берём из БД, а не из токена: токен мог быть выдан
	// давно, а профиль (например, роль) с тех пор изменился.
	u, err := h.users.GetByID(c.Request.Context(), id)
	if err != nil {
		httputil.WriteError(c, err)
		return
	}

	c.JSON(http.StatusOK, toResponse(u))
}

// ChangePassword — PATCH /me/password (защищённая ручка).
// До метода добегает только запрос с валидным access-токеном: это
// обеспечивает middleware AuthRequired на роуте.
//
// @Summary      Смена пароля
// @Description  Проверяет старый пароль, ставит новый и отзывает все refresh-сессии.
// @Tags         user
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        request body changePasswordRequest true "Старый и новый пароль"
// @Success      204 "Пароль изменён"
// @Failure      400 {object} httputil.ErrorResponse "Невалидное тело запроса"
// @Failure      401 {object} httputil.ErrorResponse "Неверный старый пароль или токен"
// @Router       /me/password [patch]
func (h *Handler) ChangePassword(c *gin.Context) {
	var req changePasswordRequest

	// Парсим тело и валидируем: пустые поля или короткий new_password → 400.
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, httputil.ErrorResponse{Error: err.Error()})
		return
	}

	// Кого меняем — берём из токена. Клиент НЕ передаёт id в теле:
	// иначе можно было бы попытаться сменить чужой пароль. Личность
	// всегда определяется токеном, а не данными запроса.
	id := auth.UserID(c)

	// ШАГ 1 — бизнес-логика: проверить старый пароль и записать новый хеш.
	// Это целиком забота user.Service.
	if err := h.users.ChangePassword(c.Request.Context(), id, req.OldPassword, req.NewPassword); err != nil {
		httputil.WriteError(c, err)
		return
	}

	// ШАГ 2 — оркестрация: разлогинить все устройства пользователя.
	// Смена пароля должна убить ранее выданные refresh-токены (пароль мог
	// утечь вместе с одним из них). Это забота auth-фичи, у которой есть
	// свой сервис сессий. Handler видит ОБА сервиса и связывает их — ровно
	// тот же приём, что в Register (users.Register + sessions.IssuePair).
	//
	// Почему здесь, а не внутри user.Service: добавь мы user.Service
	// зависимость на auth.Service — получили бы цикл при сборке в main.go
	// (auth уже строится из userService). Оркестрация в handler разрывает цикл.
	if err := h.sessions.RevokeAllForUser(c.Request.Context(), id); err != nil {
		httputil.WriteError(c, err)
		return
	}

	// 204 No Content — действие выполнено, тело ответа не нужно.
	//
	// ВАЖНОЕ ПРИМЕЧАНИЕ: сам access-токен клиента живёт ещё ~15 минут
	// (stateless, blacklist мы не ведём). Полностью "выкинуть" его до
	// истечения TTL мы не можем без доп. инфраструктуры — это осознанный
	// trade-off. А вот все refresh-сессии уже мертвы: обновиться не выйдет.
	c.Status(http.StatusNoContent)
}

// DeleteMe — DELETE /me (удаление собственного аккаунта).
// До метода доходит только запрос с валидным access-токеном.
//
// @Summary      Удаление аккаунта
// @Description  Физически удаляет пользователя; заметки и сессии уходят каскадом.
// @Tags         user
// @Security     BearerAuth
// @Success      204 "Аккаунт удалён"
// @Failure      401 {object} httputil.ErrorResponse "Нет или протух токен"
// @Router       /me [delete]
func (h *Handler) DeleteMe(c *gin.Context) {
	// Кого удаляем — определяем по токену, а не по данным запроса.
	// Иначе любой мог бы удалить чужой аккаунт, подсунув чужой id.
	id := auth.UserID(c)

	// Физическое удаление. Заметки и refresh-сессии пользователя уходят
	// каскадом на уровне БД — отдельно их трогать не нужно (см. repository.Delete).
	if err := h.users.Delete(c.Request.Context(), id); err != nil {
		httputil.WriteError(c, err)
		return
	}

	// 204 No Content — аккаунта больше нет, тело ответа не нужно.
	//
	// ПРИМЕЧАНИЕ: удалённый юзер со "старым" access-токеном формально может
	// достучаться до ручек, которые НЕ читают его из БД (AuthRequired проверяет
	// только подпись JWT, без похода в БД). Но как только ручка попробует
	// загрузить юзера (GetByID) — получит ErrNotFound → 404. Плюс refresh
	// невозможен: все refresh-токены удалены каскадом. Это тот же привычный
	// trade-off со stateless access-токеном (~15 мин), что и при смене пароля.
	c.Status(http.StatusNoContent)
}

// Политика пагинации админского списка.
// Решение "сколько отдавать по умолчанию" — это забота HTTP-слоя,
// а не сервиса или БД, поэтому константы живут здесь.
const (
	defaultLimit = 20  // если клиент не указал ?limit
	maxLimit     = 100 // совпадает с binding:"max=100" в dto.go
)

// ListUsers — GET /admin/users (только для админов).
// До метода добегает лишь запрос с валидным токеном И ролью admin:
// это обеспечивают middleware AuthRequired + RequireRole на роуте.
//
// @Summary      Список пользователей (только админ)
// @Tags         admin
// @Produce      json
// @Security     BearerAuth
// @Param        limit  query int false "Размер страницы (1..100, по умолчанию 20)"
// @Param        offset query int false "Смещение (>= 0)"
// @Success      200 {object} usersResponse
// @Failure      400 {object} httputil.ErrorResponse "Невалидные параметры"
// @Failure      401 {object} httputil.ErrorResponse "Нет или протух токен"
// @Failure      403 {object} httputil.ErrorResponse "Недостаточно прав"
// @Router       /admin/users [get]
func (h *Handler) ListUsers(c *gin.Context) {
	var q listUsersQuery

	// ShouldBindQuery — как ShouldBindJSON, но читает query string (?limit=&offset=).
	// При ошибке валидации (например, limit=999) отвечаем 400.
	if err := c.ShouldBindQuery(&q); err != nil {
		c.JSON(http.StatusBadRequest, httputil.ErrorResponse{Error: err.Error()})
		return
	}

	// Если клиент не задал limit, gin оставит 0 — подставляем дефолт.
	// (Верхнюю границу уже отсекает binding:"max=100".)
	if q.Limit == 0 {
		q.Limit = defaultLimit
	}

	users, err := h.users.List(c.Request.Context(), q.Limit, q.Offset)
	if err != nil {
		httputil.WriteError(c, err)
		return
	}

	// Отдаём не голый массив, а объект с метаданными пагинации:
	// так клиент знает, какой срез получил, и сможет запросить следующий.
	c.JSON(http.StatusOK, usersResponse{
		Users:  toResponses(users),
		Limit:  q.Limit,
		Offset: q.Offset,
	})
}
