// metrics.go — middleware учёта RED-метрик HTTP-запросов.
//
// RED: Rate (сколько запросов) — Errors (доля ошибок) — Duration (латентность).
// Это ровно то, что счётчик+гистограмма в Registry измеряют для каждого
// запроса. Хендлер /metrics в server.go отдаёт это всё Prometheus'у.
package server

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/platform/metrics"
)

// Metrics возвращает middleware, который на КАЖДЫЙ запрос увеличивает
// счётчик http_requests_total{method,path,status} и записывает длительность
// в гистограмму http_request_duration_seconds{method,path}.
//
// ПОРЯДОК в цепочке: Metrics ставится ПЕРВЫМ (снаружи RequestID/Logger/
// Recovery). Причины:
//  1. Метрики не нужны идентификатор запроса — им нечего коррелировать;
//     считать надо ВСЕ запросы, включая те, что упали в Recovery (паника →
//     ответ 500 тоже должен попасть в Errors, иначе доля ошибок занижена).
//  2. Middleware оборачивает c.Next() — значит видит финальный статус
//     ответа ПОСЛЕ того, как всё внутри (включая Recovery) отработало.
func Metrics(m *metrics.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Скрапы самого /metrics не считаем: это шум от инфраструктуры,
		// а не трафик пользователей — иначе Prometheus исказит RED-картину.
		if c.FullPath() == "/metrics" {
			c.Next()
			return
		}

		m.RequestsInFlight.Inc()
		start := time.Now()

		c.Next()

		m.RequestsInFlight.Dec()
		m.RequestsTotal.WithLabelValues(
			c.Request.Method,
			routeLabel(c),
			strconv.Itoa(c.Writer.Status()),
		).Inc()
		m.RequestDuration.WithLabelValues(
			c.Request.Method,
			routeLabel(c),
		).Observe(time.Since(start).Seconds())
	}
}

// routeLabel возвращает ШАБЛОН роута ("/api/v1/users/:id") для лейбла path.
//
// КРИТИЧНО для cardinality: если взять c.Request.URL.Path ("/api/v1/users/
// abc-123"), то каждый новый user_id рождает НОВУЮ временную серию в
// Prometheus — таблица метрик растёт неограниченно, сервис деградирует.
// Шаблон же — конечное множество (столько, сколько роутов объявлено).
//
// c.FullPath() пустой для 404 (роут не совпал) — но URL у 404 может быть
// ЧТО УГОДНО, в т.ч. бомба вариаций. Поэтому всё несматченное — одна серия
// с меткой "unmatched": виден сам факт 404-потока, без риска cardinality.
func routeLabel(c *gin.Context) string {
	if path := c.FullPath(); path != "" {
		return path
	}
	return "unmatched"
}
