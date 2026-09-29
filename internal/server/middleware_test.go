package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newTestRouter собирает мини-приложение с РЕАЛЬНОЙ цепочкой middleware
// (в боевом порядке из server.go) и двумя ручками:
//
//	/health — нормальная ручка;
//	/boom   — ручка, которая ПАНИКУЕТ (для теста Recovery).
func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	// Порядок тот же, что в server.New — тестируем именно боевую конфигурацию.
	r.Use(RequestID(), RequestLogger(), Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/boom", func(c *gin.Context) {
		panic("boom: секретная деталь внутренностей")
	})
	return r
}

// do — помощник: выполняет запрос с заданными заголовками и возвращает ответ.
func do(t *testing.T, r *gin.Engine, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// setLogCapture подменяет дефолтный slog-логер на JSON-хендлер, пишущий в
// переданный буфер. Возвращает функцию восстановления старого логера —
// зовём через defer, чтобы тест не испортил логи остальных тестов.
func setLogCapture(buf *bytes.Buffer) func() {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	return func() { slog.SetDefault(old) }
}

// =====================================================================
// RequestID
// =====================================================================

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	r := newTestRouter()

	rec := do(t, r, http.MethodGet, "/health", nil)

	// ID должен вернуться клиенту в заголовке.
	id := rec.Header().Get(RequestIDHeader)
	if id == "" {
		t.Fatal("заголовок X-Request-Id пуст — ID не выдан")
	}
	// Сгенерированный ID — это UUID: 36 символов и 4 дефиса.
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("ID не похож на UUID: %q", id)
	}
}

func TestRequestID_TrustsValidClientHeader(t *testing.T) {
	r := newTestRouter()

	// Клиент прислал свой correlation id (как это делал бы gateway).
	const clientID = "gw-correlation-id-123"
	rec := do(t, r, http.MethodGet, "/health", map[string]string{
		RequestIDHeader: clientID,
	})

	// Наш ID должен быть использован КАК ЕСТЬ, а не заменён UUID'ом —
	// иначе сопоставить логи gateway и api было бы невозможно.
	if got := rec.Header().Get(RequestIDHeader); got != clientID {
		t.Errorf("ID клиента перезаписан: получили %q, ждали %q", got, clientID)
	}
}

func TestRequestID_RejectsUnsafeClientHeader(t *testing.T) {
	r := newTestRouter()

	tests := []struct {
		name string
		id   string
	}{
		{"слишком длинный (раздувание логов)", strings.Repeat("a", maxRequestIDLen+1)},
		{"с пробелом (log injection)", "fake entry\n2026-01-01 ERROR fake line"},
		{"с переводом строки", "id\nnewline"},
		{"не-ASCII символы", "idéé"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, r, http.MethodGet, "/health", map[string]string{
				RequestIDHeader: tt.id,
			})

			// Небезопасный ID обязан быть подменён сгенерированным UUID'ом,
			// а не прокинут в ответ/логи как есть.
			got := rec.Header().Get(RequestIDHeader)
			if got == tt.id {
				t.Errorf("опасный ID %q не был подменён", got)
			}
			if len(got) != 36 || strings.Count(got, "-") != 4 {
				t.Errorf("подмена должна быть UUID'ом, получили %q", got)
			}
		})
	}
}

// =====================================================================
// RequestLogger
// =====================================================================

func TestRequestLogger_WritesStructuredLine(t *testing.T) {
	r := newTestRouter()

	// Перенаправляем дефолтный slog в буфер и восстанавливаем по defer.
	var buf bytes.Buffer
	restore := setLogCapture(&buf)
	defer restore()

	const clientID = "test-req-id-1"
	rec := do(t, r, http.MethodGet, "/health", map[string]string{
		RequestIDHeader: clientID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("получили %d, ждали 200", rec.Code)
	}

	// Логер пишет ОДНУ JSON-строку. Разбираем её в map и сверяем поля —
	// это ровно то, как строку будет читать log-collector.
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("лог-строка не записана")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("лог не JSON: %v; строка: %s", err, line)
	}

	if entry["msg"] != "http request" {
		t.Errorf("msg: %v", entry["msg"])
	}
	if entry["request_id"] != clientID {
		t.Errorf("request_id: %v, ждали %q (ID должен проходить из middleware в лог)", entry["request_id"], clientID)
	}
	if entry["method"] != http.MethodGet {
		t.Errorf("method: %v", entry["method"])
	}
	if entry["path"] != "/health" {
		t.Errorf("path: %v", entry["path"])
	}
	// JSON-числа unmarshal'ятся во float64.
	if entry["status"].(float64) != http.StatusOK {
		t.Errorf("status: %v", entry["status"])
	}
	if _, ok := entry["duration"]; !ok {
		t.Error("нет поля duration")
	}
}

func TestRequestLogger_LevelByStatus(t *testing.T) {
	r := newTestRouter()

	var buf bytes.Buffer
	restore := setLogCapture(&buf)
	defer restore()

	// 404 (нет такой ручки) — ошибка КЛИЕНТА: лог должен быть WARN.
	do(t, r, http.MethodGet, "/no-such-route", nil)

	line := strings.TrimSpace(buf.String())
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("лог не JSON: %v", err)
	}
	if entry["level"] != "WARN" {
		t.Errorf("уровень для 4xx: %v, ждали WARN", entry["level"])
	}
	if entry["status"].(float64) != http.StatusNotFound {
		t.Errorf("status: %v", entry["status"])
	}
}

// =====================================================================
// Recovery
// =====================================================================

func TestRecovery_ConvertsPanicTo500(t *testing.T) {
	r := newTestRouter()

	// Паника в хендлере НЕ должна ронять процесс: клиент получает
	// корректный JSON 500, а не оборванное соединение.
	rec := do(t, r, http.MethodGet, "/boom", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("получили %d, ждали 500; тело: %s", rec.Code, rec.Body.String())
	}

	// Тело — в едином формате ошибок API, с ОБЩИМ текстом:
	// детали паники ("секретная деталь внутренностей") наружу не утекают.
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v; тело: %s", err, rec.Body.String())
	}
	if body.Error != "internal error" {
		t.Errorf("error: %q, ждали обобщённое \"internal error\"", body.Error)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("детали паники просочились клиенту!")
	}
}

func TestRecovery_PanicStillLoggedWithRequestID(t *testing.T) {
	r := newTestRouter()

	var buf bytes.Buffer
	restore := setLogCapture(&buf)
	defer restore()

	do(t, r, http.MethodGet, "/boom", nil)

	// Паника должна быть залогирована Recovery'ем на уровне ERROR,
	// со стеком и request_id — иначе её не найти в логах.
	if !strings.Contains(buf.String(), "panic recovered") {
		t.Errorf("паника не залогирована; буфер: %s", buf.String())
	}

	// В буфере ДВЕ JSON-строки (это нормально): сначала Recovery пишет
	// "panic recovered", затем — уже после перехвата паники — Logger
	// дописывает access-строку запроса со статусом 500. Поэтому парсим
	// строки по отдельности и ищем ту, что про панику.
	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("лог не JSON: %v; строка: %s", err, line)
		}
		if candidate["msg"] == "panic recovered" {
			entry = candidate
			break
		}
	}
	if entry == nil {
		t.Fatal("запись panic recovered не найдена в логе")
	}
	if entry["level"] != "ERROR" {
		t.Errorf("уровень: %v, ждали ERROR", entry["level"])
	}
	if _, ok := entry["stack"]; !ok {
		t.Error("нет поля stack — место падения не будет видно")
	}
	if _, ok := entry["request_id"]; !ok {
		t.Error("нет поля request_id — панику не привязать к запросу")
	}
}

// TestRecovery_AfterPanicServerServesNextRequest — паника не должна ломать
// сервер целиком: следующий запрос обрабатывается как ни в чём не бывало.
func TestRecovery_AfterPanicServerServesNextRequest(t *testing.T) {
	r := newTestRouter()

	do(t, r, http.MethodGet, "/boom", nil) // паникуем
	rec := do(t, r, http.MethodGet, "/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("после паники сервер не отвечает: %d", rec.Code)
	}
}
