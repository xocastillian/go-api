package server

import (
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// CORS собирает middleware кросс-доменных запросов для браузерного фронтенда.
//
// Напоминание про то, что делает CORS:
//   - Simple request: браузер шлёт запрос, но ПРОВЕРЯЕТ ответный заголовок
//     Access-Control-Allow-Origin. Нет нашего origin'а → ответ отбрасывается.
//   - Preflight: для "непростых" запросов (метод PATCH/DELETE, заголовок
//     Authorization, Content-Type: application/json) браузер СНАЧАЛА шлёт
//     автоматический OPTIONS-запрос с вопросом "а это вообще разрешено?".
//     Именно поэтому middleware нужно вешать ГЛОБАЛЬНО и ДО аутентификации:
//     у preflight-запроса нет ни токена, ни тела, и ему нечего проверять в auth.
//
// Мы берём проверенную библиотеку gin-contrib/cors, а не пишем руками:
// в ручной валидации origin'а легко допустить дыру (например, разрешить
// лишнее или напутать с Vary: Origin), а цена ошибки тут — безопасность.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	// Пустой список = CORS выключен ("запрещено всё, что не разрешено явно").
	// НО: у gin-contrib/cors состояния "выключено" нет — на пустом AllowOrigins
	// он ПАНИКУЕТ при создании middleware ("all origins disabled"). Поэтому
	// выключенное состояние реализуем здесь сами: middleware-заглушка.
	// "Выключенный" CORS всё равно безопасен для сервера: без ответных
	// заголовков Access-Control-Allow-Origin браузер САМ отбросит
	// кросс-доменные ответы. (Сервер при этом никуда не защищается — CORS
	// вообще не про это, см. докблок выше.)
	// Латентный баг: до этой правки пустой конфиг падал при СТАРТЕ, но в
	// dev-дефолте список всегда был непустой — и баг прятался.
	if len(allowedOrigins) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	return cors.New(cors.Config{
		// Явный белый список origin'ов. Пустой список => CORS выключен,
		// браузерные запросы с других доменов блокируются. Это и есть
		// безопасный дефолт — "запрещено всё, что не разрешено явно".
		AllowOrigins: allowedOrigins,

		// Какие HTTP-методы разрешены. Включаем OPTIONS для preflight.
		AllowMethods: []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},

		// Какие заголовки клиент может прислать. Content-Type и Authorization —
		// именно они делают запрос "непростым" и требуют preflight.
		AllowHeaders: []string{"Origin", "Content-Type", "Authorization"},

		// Какие заголовки ответа JS может прочитать. Нам достаточно базового.
		ExposeHeaders: []string{"Content-Length"},

		// Мы аутентифицируемся через Authorization: Bearer, а НЕ через cookie,
		// поэтому передавать credentials (cookie) не нужно. Если в будущем
		// refresh-токен переедет в HttpOnly-cookie — переключить на true.
		AllowCredentials: false,

		// Сколько секунд браузер может кэшировать результат preflight-запроса,
		// чтобы не слать OPTIONS перед каждым запросом. 12 часов — разумно.
		MaxAge: 12 * time.Hour,
	})
}
