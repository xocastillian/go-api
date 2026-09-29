// recovery.go — middleware, перехватывающий паники в хендлерах.
//
// Зачем нужен: паника в одном хендлере НЕ должна ронять весь процесс
// (а net/http без перехвата закрывает соединение с сырой простынёй в
// stderr — без request_id, без JSON-ответа, без наших полей).
//
// Это замена штатного gin.Recovery(): тот пишет панику в дефолтный лог
// через gin-овый механизм, а мы хотим СВОЙ формат — slog-JSON с request_id,
// со стеком и в общий пайплайн логов, где уже лежат access-логи.
package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/httputil"
)

// Recovery — middleware паники. Ставится ВНУТРИ RequestLogger (см. порядок
// в server.go): паника в хендлере гасится ЗДЕСЬ, после чего Logger
// дорабатывает и корректно логирует статус 500. Поменяй их местами — и
// паникующие запросы перестанут попадать в access-лог со статусом.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		// defer с recover() — единственный способ перехватить панику в Go.
		// Работает так: паника в хендлере разматывает стек; на unwind'е
		// выполняется этот defer, recover() возвращает значение паники
		// (и гасит её), функция Recovery завершается нормально.
		defer func() {
			r := recover()
			if r == nil {
				return // паники не было — обычный завершение запроса
			}

			// http.ErrAbortHandler — специальный sentinel: net/http использует
			// его для "тихого" обрыва соединения, и по конвенции его НЕЛЬЗЯ
			// перехватывать логированием, нужно пробросить дальше. Без этого
			// крайние случаи (обрыв клиента во время ответа) получат 500-тело,
			// которого не должно быть.
			//
			// Сравнение через errors.Is, а НЕ через == (как это делает и
			// сам gin.Recovery): паника может нести ошибку, обёрнутую в
			// fmt.Errorf("%w", ...), — "==" её бы не распознала, а errors.Is
			// разворачивает цепочку. Значение паники — тип any, поэтому
			// сначала type assertion: errors.Is работает только с error.
			if err, ok := r.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(r)
			}

			// r — значение ЛЮБОГО типа (panic(42) валиден), поэтому
			// превратим его в строку через %v, а не через type assertion.
			// debug.Stack() — стек на момент паники: без него лог бесполезен,
			// потому что не покажет, ГДЕ именно упало (файл:строка).
			slog.Error("panic recovered",
				"request_id", RequestIDOf(c),
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"err", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()),
			)

			// Пишем клиенту только обобщённое сообщение: деталей паники НЕ
			// отдаём (это же правило в httputil.WriteError для 500-х) —
			// текст паники может раскрыть внутренности приложения.
			// Формат ErrorResponse тот же, что у всех ошибок API: {"error": "..."}.
			//
			// Проверка Written(): если заголовки уже ушли клиенту (паника
			// случилась ПОСЛЕ начала записи тела ответа), второй раз
			// WriteHeader вызвать нельзя — http.Server сам разорвёт
			// соединение. В этом случае просто прерываем цепочку.
			if !c.Writer.Written() {
				c.AbortWithStatusJSON(http.StatusInternalServerError, httputil.ErrorResponse{Error: "internal error"})
			} else {
				c.Abort()
			}
		}()

		c.Next()
	}
}
