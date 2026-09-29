// Package metrics — сбор и экспорт метрик Prometheus.
//
// Зачем отдельный пакет: метрики — инфраструктурная забота, не бизнес-логика
// и не забота конкретной фичи (user/auth). Они лежат в platform/ рядом с
// postgres по тому же принципу: платформенные детали, которые фичи
// ПОТРЕБЛЯЮТ через маленький интерфейс, а не импортируют напрямую.
//
// Почему свой реестр, а не prometheus.DefaultRegisterer (глобальный):
// глобальные переменные — скрытая связь между частями программы. С
// собственным реестром Registry создаётся в composition root (main.go),
// передаётся тем, кому нужен, и ЖИВЁТ РОВНО В ДВУХ МЕСТАХ: middleware
// пишет в него, /metrics отдаёт. Тест может создать СВОЙ реестр и ничего
// не подхватить от соседних тестов.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry — набор метрик приложения + реестр, в котором они зарегистрированы.
// Один экземпляр на процесс (создаёт main.go), зависимости получает через DI.
type Registry struct {
	reg *prometheus.Registry

	// RequestsTotal — счётчик запросов по method/path/status.
	// Имя обязательной конвенции Prometheus: _total суффикс для Counter.
	// Лейблы — ТОЛЬКО ограниченные множества (см. про cardinality в middleware).
	RequestsTotal *prometheus.CounterVec

	// RequestDuration — гистограмма длительности по method/path (секунды).
	// По нему PromQL считает перцентили: histogram_quantile(0.99, rate(...)).
	RequestDuration *prometheus.HistogramVec

	// RequestsInFlight — gauge: сколько запросов обслуживается ПРЯМО СЕЙЧАС.
	// Полезно как термометр перегрузки: растёт и не падает — что-то застряло.
	RequestsInFlight prometheus.Gauge
}

// New создаёт реестр со всеми метриками приложения.
func New() *Registry {
	// Собственный реестр вместо дефолтного — см. докблок пакета.
	reg := prometheus.NewRegistry()

	m := &Registry{
		reg: reg,
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "http_requests_total",
				Help: "Total number of HTTP requests processed.",
			},
			// Порядок лейблов запоминается: WithLabelValues принимает
			// значения В ЭТОМ порядке (method, path, status).
			[]string{"method", "path", "status"},
		),
		RequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "http_request_duration_seconds",
				Help: "HTTP request duration in seconds.",
				// Полочки гистограммы = разрешение перцентилей, которые мы
				// сможем посчитать. Дефолтные полочки client_golang подогнаны
				// под веб (от 5ms до 10s) — начинаем с них; когда узнаешь
				// реальные латентности сервиса, полочки можно перенастроить.
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "path"},
		),
		RequestsInFlight: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "http_requests_in_flight",
				Help: "Number of HTTP requests currently being processed.",
			},
		),
	}

	// Регистрируем свои метрики...
	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.RequestsInFlight,
		// ...и два СТАНДАРТНЫХ сборщика, которые тянет почти каждый Go-сервис:
		// GoCollector — рантайм: число горутин, память GC, время GC. Первый
		// симптом утечки горутин виден именно тут (goroutines растут).
		// ProcessCollector — ресурсы процесса ОС: RSS, CPU, открытые fd.
		// (Версии из корня prometheus deprecated — берём из collectors.)
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler отдаёт http.Handler эндпоинта /metrics: он сериализует все
// зарегистрированные метрики в text format Prometheus при каждом запросе.
// Сериализация НЕ кэшируется: Prometheus скрапит раз в N секунд, число
// маленькое, дешёво. (HTTP-интерфейс отделяет реестр от gin: metrics
// про Prometheus, а не про веб-фреймворк.)
func (m *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
