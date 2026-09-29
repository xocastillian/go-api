package user

import (
	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/auth"
)

// RegisterRoutes вешает роуты пользователя на переданную группу.
// Полный путь складывается из префикса группы (его задаёт сервер)
// и путей ниже: /<prefix>/auth/register, /login, /me.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// Публичные ручки: сюда токен НЕ нужен.
	// Переменную зовём authGroup, чтобы не затереть имя импортированного пакета auth.
	authGroup := rg.Group("/auth")
	authGroup.POST("/register", h.Register)
	authGroup.POST("/login", h.Login)

	// Защищённая ручка: /me доступна только с валидным Bearer-токеном.
	// AuthRequired — middleware; передаём ему менеджер токенов, который
	// handler уже держит. Один TokenManager используется и для выпуска
	// (в Login), и для проверки (здесь) — это и есть его назначение.
	rg.GET("/me", auth.AuthRequired(h.tokens), h.Me)

	// Смена пароля — тоже "про себя", поэтому под /me и под той же защитой.
	// PATCH, а не PUT: меняем лишь ЧАСТЬ ресурса (пароль), а не пользователя
	// целиком. Семантически корректнее и честнее для клиента.
	rg.PATCH("/me/password", auth.AuthRequired(h.tokens), h.ChangePassword)

	// Удаление собственного аккаунта. DELETE /me — идиоматичный способ
	// сказать "удали ресурс, которым являюсь я". Физическое, необратимое.
	rg.DELETE("/me", auth.AuthRequired(h.tokens), h.DeleteMe)

	// Админская группа. Middleware применяются ко ВСЕМ роутам группы,
	// поэтому защита объявляется один раз — новые эндпоинты унаследуют её
	// автоматически, и забыть закрыть ручку станет невозможно.
	//
	// ПОРЯДОК КРИТИЧЕН: сначала AuthRequired (устанавливает "кто ты" и
	// кладёт роль в контекст), затем RequireRole (читает роль оттуда
	// и проверяет "можно ли тебе"). Перепутаешь — RequireRole увидит
	// пустую роль и отклонит вообще всех, включая админа.
	admin := rg.Group("/admin",
		auth.AuthRequired(h.tokens),
		auth.RequireRole(string(RoleAdmin)),
	)
	// Полный путь: /<prefix>/admin/users. RoleAdmin — типизированная роль,
	// а RequireRole принимает string (пакет auth не знает про домен user).
	admin.GET("/users", h.ListUsers)
}
