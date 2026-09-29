package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Server инкапсулирует HTTP-сервер и его роутер.
type Server struct {
	http   *http.Server
	router *gin.Engine
}

// New собирает сервер: роутер gin, middleware, роуты, настройки http.Server.
//
// corsOrigins — белый список origin'ов для кросс-доменных запросов
// (передаётся из конфига; см. server/cors.go). Пустой список = CORS выключен.
func New(port string, corsOrigins []string) *Server {
	router := gin.New()

	// --- Цепочка middleware. ПОРЯДОК КРИТИЧЕН, читается снаружи внутрь: ---
	// каждый запрос проходит: RequestID -> RequestLogger -> Recovery -> CORS
	// -> хендлер, а возвращается в обратном порядке. Почему именно так:
	//
	// 1. RequestID — САМЫЙ внешний. Все, кто ниже (Logger, Recovery), должны
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
	router.Use(RequestID())
	router.Use(RequestLogger())
	router.Use(Recovery())
	router.Use(CORS(corsOrigins))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

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
