package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Ключи, под которыми кладём данные в контекст gin.
// Префикс "auth." защищает от коллизий: в gin.Context ключи — это строки,
// и любой другой пакет тоже может что-то туда положить. Уникальные имена
// гарантируют, что мы не перетрём чужое значение (и наоборот).
const (
	ctxUserIDKey = "auth.user_id"
	ctxRoleKey   = "auth.role"
)

// AuthRequired — middleware аутентификации.
// Пропускает запрос дальше только если есть валидный Bearer-токен.
// Иначе прерывает цепочку и отвечает 401.
//
// ВАЖНО: это middleware АУТЕНТИФИКАЦИИ (кто ты) — оно только устанавливает
// личность. За "что тебе можно" отвечает отдельное RequireRole (авторизация).
func AuthRequired(tm *TokenManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Ожидаем заголовок строго вида: "Authorization: Bearer <token>".
		const prefix = "Bearer "

		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, prefix) {
			// 401 Unauthorized — клиент не представился (нет токена).
			// AbortWithStatusJSON не только пишет ответ, но и ПРЕРЫВАЕТ
			// выполнение: следующие хендлеры не вызовутся. Без Abort
			// запрос пошёл бы дальше как ни в чём не бывало.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}

		// Отрезаем префикс "Bearer " и лишние пробелы — остаётся сам токен.
		tokenString := strings.TrimSpace(strings.TrimPrefix(header, prefix))

		// Parse проверит подпись и срок. Любая проблема → ошибка.
		claims, err := tm.Parse(tokenString)
		if err != nil {
			// Один и тот же ответ на "нет токена"/"просрочен"/"подделан":
			// клиенту не нужно знать причину, а атакующему — тем более.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		// Кладём личность в контекст. Отсюда её достанут хендлеры через
		// UserID(c) / Role(c). Ключ — наш, приватный, с префиксом.
		c.Set(ctxUserIDKey, claims.UserID)
		c.Set(ctxRoleKey, claims.Role)

		// c.Next() передаёт управление следующему в цепочке (хендлеру).
		// Без него запрос "зависнет" на этом middleware.
		c.Next()
	}
}

// RequireRole — middleware авторизации по роли.
// Ставится ПОСЛЕ AuthRequired (иначе роли в контексте ещё нет).
// Если роль из токена не совпадает с требуемой — 403 Forbidden.
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if Role(c) != role {
			// 403 Forbidden — клиент известен, но прав недостаточно.
			// (401 = "кто ты?", 403 = "тебе нельзя".)
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}

// UserID достаёт id пользователя из контекста (после AuthRequired).
// Если значения нет — вернёт нулевой UUID: это сигнал, что ручку забыли
// закрыть middleware'ом.
func UserID(c *gin.Context) uuid.UUID {
	v, _ := c.Get(ctxUserIDKey)
	id, _ := v.(uuid.UUID) // безопасное приведение типа: при промахе получим zero-value
	return id
}

// Role достаёт роль пользователя из контекста.
func Role(c *gin.Context) string {
	v, _ := c.Get(ctxRoleKey)
	role, _ := v.(string)
	return role
}
