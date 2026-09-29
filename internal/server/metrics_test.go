package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/platform/metrics"
)

// newMetricsRouter собирает мини-приложение с БОЕВОЙ цепочкой (Metrics
// первым — как в server.New) и ручками, покрывающими все ветки middleware:
//
//	/api/v1/things/:id — нормальная ручка с параметром (проверка FullPath);
//	/boom              — паникующая (проверка, что 500 от Recovery считается);
//	/readyz            — заглушка readiness для тестов health-эндпоинтов.
func newMetricsRouter(m *metrics.Registry, readyFn ReadyChecker) *gin.Engine {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(Metrics(m), RequestID(), RequestLogger(), Recovery())

	r.GET("/api/v1/things/:id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
	})
	// Эндпоинт /metrics обычно вешает server.New — здесь регистрируем
	// вручную, чтобы readMetric мог забрать дамп метрик из этого роутера.
	r.GET("/metrics", gin.WrapH(m.Handler()))
	r.GET("/boom", func(c *gin.Context) {
		panic("boom")
	})
	r.GET("/readyz", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

// readMetric достаёт из ответа /metrics значение строки с префиксом want.
// Парсим подстрокой, а не полным expfmt-парсером: нас интересует сам факт
// наличия серии с нужными лейблами и её значение, а это проще и нагляднее.
// Формат строки строго: name{label="v",...} value
func readMetric(t *testing.T, r *gin.Engine, prefix string) (string, bool) {
	t.Helper()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

// TestMetrics_CountsRequestsAndUsesRouteTemplate — счётчик растёт на каждый
// запрос, а в лейбле path стоит ШАБЛОН роута, а не конкретный URL: два
// запроса с разными :id должны попасть в ОДНУ серию (cardinality!).
func TestMetrics_CountsRequestsAndUsesRouteTemplate(t *testing.T) {
	m := metrics.New()
	r := newMetricsRouter(m, nil)

	do(t, r, http.MethodGet, "/api/v1/things/aaa", nil)
	do(t, r, http.MethodGet, "/api/v1/things/bbb", nil)

	// Найденная строка целиком: имя + лейблы + значение.
	line, ok := readMetric(t, r, `http_requests_total{method="GET",path="/api/v1/things/:id",status="200"}`)
	if !ok {
		t.Fatalf("серия http_requests_total с шаблоном роута не найдена")
	}
	if !strings.HasSuffix(line, " 2") {
		t.Fatalf("ожидали счётчик 2 (два запроса, одна серия!), got: %s", line)
	}

	// Гистограмма обязана тоже среагировать: суммарное время наблюдений > 0.
	dur, ok := readMetric(t, r, `http_request_duration_seconds_sum{method="GET",path="/api/v1/things/:id"}`)
	if !ok || strings.HasSuffix(dur, " 0") {
		t.Fatalf("гистограмма длительности не записала наблюдения: %q", dur)
	}
}

// TestMetrics_404IsUnmatchedSeries — несматченные URL (404) должны попадать
// в ОДНУ серию path="unmatched", какие бы вариативные пути ни присылали.
func TestMetrics_404IsUnmatchedSeries(t *testing.T) {
	m := metrics.New()
	r := newMetricsRouter(m, nil)

	do(t, r, http.MethodGet, "/nope/xyz", nil)

	_, ok := readMetric(t, r, `http_requests_total{method="GET",path="unmatched",status="404"}`)
	if !ok {
		t.Fatalf("ожидали серию 404 с path=\"unmatched\"")
	}
}

// TestMetrics_PanicCountedAs500 — запрос, погашенный Recovery, обязан
// попасть в счётчик как 500: иначе Errors в RED занижены.
func TestMetrics_PanicCountedAs500(t *testing.T) {
	m := metrics.New()
	r := newMetricsRouter(m, nil)

	do(t, r, http.MethodGet, "/boom", nil)

	if _, ok := readMetric(t, r, `http_requests_total{method="GET",path="/boom",status="500"}`); !ok {
		t.Fatalf("паниковавший запрос не попал в метрики как 500")
	}
}

// TestMetrics_MetricsEndpointNotCounted — скрапы /metrics не попадают в
// счётчик пользовательского трафика: иначе Prometheus сам себе раздувает
// Rate, который мы потом строим из этого счётчика.
func TestMetrics_MetricsEndpointNotCounted(t *testing.T) {
	m := metrics.New()
	r := newMetricsRouter(m, nil)

	do(t, r, http.MethodGet, "/metrics", nil)
	do(t, r, http.MethodGet, "/metrics", nil)

	if _, ok := readMetric(t, r, `http_requests_total{method="GET",path="/metrics"`); ok {
		t.Fatalf("скрапы /metrics не должны считаться пользовательским трафиком")
	}
}
