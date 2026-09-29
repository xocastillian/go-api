package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/timqa/my-rest-api/internal/platform/metrics"
)

// =====================================================================
// Health endpoints: /healthz (liveness) и /readyz (readiness)
// =====================================================================
//
// Тестируем через РЕАЛЬНЫЙ server.New — здесь важна не только логика
// хендлеров, но и то, что они корректно зарегистрированы в роутере.

// newHealthServer собирает настоящий Server с проверкой готовности,
// управляемой каналом: тест может переключать её состояние изhealthy в
// unhealthy посреди теста. nil-канал = всегда готов.
func newHealthServer(t *testing.T, readyCh chan error) *Server {
	t.Helper()
	return New("0", nil, func(ctx context.Context) error {
		if readyCh == nil {
			return nil
		}
		select {
		case err := <-readyCh:
			return err
		default:
			return nil
		}
	}, metrics.New())
}

func TestHealthz_AlwaysOk(t *testing.T) {
	s := newHealthServer(t, nil)

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz должен быть 200, пока процесс жив, got %d", rec.Code)
	}
}

func TestReadyz_OkWhenReady(t *testing.T) {
	s := newHealthServer(t, nil)

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("readyz при готовой БД должен быть 200, got %d", rec.Code)
	}
}

// TestReadyz_503WhenDependencyDown — сердце различия liveness/readiness:
// зависимость упала → 503 (убрать из ротации трафика), а НЕ 500 и не
// падение процесса. Оркестратор не должен перезапускать здоровый процесс.
func TestReadyz_503WhenDependencyDown(t *testing.T) {
	readyCh := make(chan error, 1)
	readyCh <- errors.New("db down")
	s := newHealthServer(t, readyCh)

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz при недоступной БД должен быть 503, got %d", rec.Code)
	}
}
