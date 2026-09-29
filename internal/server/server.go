package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/timqa/my-rest-api/internal/platform/metrics"
)

// ReadyChecker — функция проверки готовности сервиса для /readyz.
// Возвращает nil, если сервис может обслуживать трафик, иначе ошибку.
// Сигнатура — тип, а не просто параметр func(...): имя типа документирует
// НАЗНАЧЕНИЕ функции в сигнатуре New и подскажет IDE при наведении.
type ReadyChecker func(ctx context.Context) error

// readyTimeout — сколько ждём readyFn перед тем, как отдать 503. Healthcheck
// должен отвечать быстро и предсказуемо: оркестратор пингует его часто, и
// медленный ответ сам по себе читается как "не готов".
const readyTimeout = 3 * time.Second

// Server инкапсулирует HTTP-сервер и его роутер.
type Server struct {
	http   *http.Server
	router *gin.Engine
}

// New собирает сервер: роутер gin, middleware, роуты, настройки http.Server.
//
// corsOrigins — белый список origin'ов для кросс-доменных запросов
// (передаётся из конфига; см. server/cors.go). Пустой список = CORS выключен.
//
// readyFn — проверка ГОТОВНОСТИ сервиса для /readyz (см. ниже). Сервер сам
// не знает про БД: он принимает функцию, а ЧТО проверять решает composition
// root (main.go подставит ping пула). Сервер остаётся чистым от зависимостей.
func New(port string, corsOrigins []string, readyFn ReadyChecker, m *metrics.Registry) *Server {
	router := gin.New()

	// --- Цепочка middleware. ПОРЯДОК КРИТИЧЕН, читается снаружи внутрь: ---
	// каждый запрос проходит: Metrics -> RequestID -> RequestLogger ->
	// Recovery -> CORS -> хендлер, а возвращается в обратном порядке.
	// Почему именно так:
	//
	// 0. Metrics — САМЫЙ внешний. Ему нужен ФИНАЛЬНЫЙ статус ответа, значит
	//    он должен обёртывать ВСЁ, включая Recovery: запрос, упавший паникой,
	//    обязан попасть в http_requests_total со статусом 500, иначе доля
	//    ошибок (E в RED) занижена. Метрики ничего не требуют от соседей —
	//    им не нужен request id, поэтому они могут стоять раньше всех.
	//
	// 1. RequestID — теперь второй. Все, кто ниже (Logger, Recovery), должны
	//    уже видеть ID запроса, иначе логи и записи о паниках останутся без
	//    корреляции.
	//
	// 2. RequestLogger — ДО Recovery. Может показаться странным, но причина
	//    в порядке размотки паники: Recovery стоит ВНУТРИ Logger'а, значит
	//    ГАСИТ панику раньше, чем она долетит до Logger'а. После Recovery
	//    цепочка продолжается штатно, Logger записывает ответ со статусом
	//    500 — и паникующие запросы попадают в access-лог. Переставь их
	//    местами: паника пролетит сквозь Logger, тот не успеет ничего
	//    залогировать, и запрос исчезнет из логов бесследно.
	//    (Сам Logger паниковать не может — он только читает и пишет лог.)
	//
	// 3. Recovery — последняя линия обороны процесса: гасит паники хендлеров
	//    и middleware ниже, отвечает клиенту 500 и пишет стек в slog.
	//
	// 4. CORS — глобально и ДО роутов, чтобы перехватывать preflight-запросы
	//    (OPTIONS) в том числе на защищённые ручки: у preflight нет токена,
	//    и он не должен доходить до AuthRequired.
	router.Use(Metrics(m))
	router.Use(RequestID())
	router.Use(RequestLogger())
	router.Use(Recovery())
	router.Use(CORS(corsOrigins))

	// --- Инфраструктурные эндпоинты ---
	// НЕ часть /api/v1 и не бизнес-ручки: их клиент — не пользователь, а
	// оркестратор (docker/k8s) и Prometheus.
	//
	// /healthz — LIVENESS: «жив ли процесс». Всегда 200, пока процесс
	// может ответить. Оркестратор по нему решает: убить и перезапустить
	// контейнер (завис/дедлок). Никаких зависимостей тут НЕ проверяем:
	// если liveness начнёт падать от недоступной БД, оркестратор будет
	// бесконечно рестартить процесс, который в порядке.
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// /readyz — READINESS: «может ли сервис обслуживать трафик». Вот здесь
	// проверяем зависимости (пинг БД). Если БД лежит — 503, и оркестратор/
	// балансер выведут инстанс из ротации, но НЕ убьют процесс: он здоров,
	// трафик просто нельзя пускать. Ответ на 503 оставляем «тупым» (без
	// деталей об ошибке БД): эндпоинт публичный.
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), readyTimeout)
		defer cancel()

		if err := readyFn(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	// /metrics — отдача метрик в text format Prometheus. HTTP-хендлер из
	// пакета metrics адаптируем в gin через WrapH: metrics не знает про gin.
	router.GET("/metrics", gin.WrapH(m.Handler()))

	return &Server{
		router: router,
		http: &http.Server{
			Addr:         ":" + port,
			Handler:      router,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
	}
}

// Router отдаёт корневой роутер, чтобы main мог навесить группы фич.
// Сервер ничего не знает про user/note — связывание происходит снаружи,
// в композиционном корне (main).
func (s *Server) Router() *gin.Engine {
	return s.router
}

// Run запускает сервер и блокирует выполнение, пока он слушает.
// Возвращает ошибку только если сервер НЕ смог стартовать.
func (s *Server) Run() error {
	slog.Info("http server listening", "addr", s.http.Addr)

	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown плавно останавливает сервер: перестаёт принимать новые соединения
// и ждёт завершения уже активных запросов, пока не истечёт ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
