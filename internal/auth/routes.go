package auth

import "github.com/gin-gonic/gin"

// RegisterRoutes вешает ручки сессии на переданную группу.
// Подгруппа /auth мёржится с /auth из фичи user: итоговые пути —
// /<prefix>/auth/refresh, /<prefix>/auth/logout, /<prefix>/auth/logout-all.
//
// Refresh и Logout ПУБЛИЧНЫЕ (без AuthRequired): /refresh вызывают, когда
// access-токен протух, /logout — тоже без него. Защита здесь — сам
// refresh-токен.
//
// LogoutAll — ЗАЩИЩЁННАЯ: «выйти со всех устройств» — действие владельца,
// и его личность берём из access-токена, а не из тела. Приём тот же, что
// в фиче user (rg.GET("/me", auth.AuthRequired(h.tokens), h.Me)), только
// AuthRequired лежит в своём пакете, поэтому без квалификатора.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	authGroup := rg.Group("/auth")
	authGroup.POST("/refresh", h.Refresh)
	authGroup.POST("/logout", h.Logout)
	authGroup.POST("/logout-all", AuthRequired(h.tokens), h.LogoutAll)
}
