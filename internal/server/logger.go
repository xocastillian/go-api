// logger.go — middleware access-логов: одна JSON-строка на каждый HTTP-запрос.
//
// Это называется "access log" — как в nginx. Для каждого запроса логируем:
// кто (ip), что (method, path), чем закончилось (status) и за сколько
// (duration). В JSON-формат, который уже настроен в main.go (slog.NewJSONHandler)
// — парсится любым log-collector'ом (Loki, ELK) без регулярок.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestLogger — middleware access-логов. Ставится ПОСЛЕ RequestID,
// чтобы каждая строка лога содержала request_id.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Стартовое время — до всего остального: меряем ВЕСЬ путь запроса.
		start := time.Now()

		// defer — логируем ПОСЛЕ завершения запроса, при любом сценарии:
		// нормальном возврате или AbortWithStatusJSON. Панику сюда не занесёт:
		// Recovery стоит ВНУТРИ (ниже по цепочке) и гасит её раньше.
		defer func() {
			// Финальный статус берём у самого Writer — интерфейс
			// gin.ResponseWriter имеет метод Status().
			//
			// ПОДВОХ, который здесь учтён: gin для "нет такого роута" (404)
			// и паники net/http пишет статус НАПРЯМУЮ во внутренний writer,
			// минуя цепочку — самодельная обёртка, перехватывающая
			// WriteHeader, эти случаи пропустила бы (увидела бы 200).
			// У встроенного Writer статус учтён всегда.
			//
			// Дефолт 200: Writer, в который ничего не писали, отчитывается
			// именно нулевым статусом... нет — 200-м: gin хранит дефолт 200
			// с самого старта (см. gin.responseWriter.status = http.StatusOK).
			status := c.Writer.Status()
			if status == 0 { // теоретически возможен только у самодельного Writer
				status = http.StatusOK
			}

			// Уровень лога зависит от статуса — так grep'ом мгновенно
			// выделяются проблемные запросы:
			//   5xx → ERROR (наша ошибка, срочно смотреть)
			//   4xx → WARN  (ошибка клиента: боты, кривые фронтенды, атаки)
			//   3xx/2xx → INFO (нормальная жизнь)
			level := slog.LevelInfo
			switch {
			case status >= 500:
				level = slog.LevelError
			case status >= 400:
				level = slog.LevelWarn
			}

			// ВАЖНО: контекст — Background(), а НЕ c.Request.Context().
			// К моменту логирования запрос уже завершён, и его контекст
			// отменён. slog в дефолтной конфигурации контекст игнорирует,
			// но если позже подключить хендлер, берущий из контекста
			// trace_id — с отменённым контекстом трейсы потерялись бы.
			// Background() всегда живой.
			slog.LogAttrs(context.Background(), level, "http request",
				slog.String("request_id", RequestIDOf(c)), // тот самый ID из requestid.go
				slog.String("method", c.Request.Method),
				slog.String("path", c.Request.URL.Path),
				slog.Int("status", status),
				// slog.Duration напечатает человекочитаемо: "12.5ms", "1.3s".
				// duration.Milliseconds() не подошёл бы: он целочисленный, и
				// быстрые запросы (< 1ms) выглядели бы как 0.
				slog.Duration("duration", time.Since(start)),
				slog.String("ip", c.ClientIP()),
			)
		}()

		c.Next()
	}
}
