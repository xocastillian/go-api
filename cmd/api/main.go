// @title           My REST API
// @BasePath        /api/v1
//
// @securityDefinitions.apikey BearerAuth
// @in              header
// @name            Authorization
// @description     Bearer <access_token>
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	_ "github.com/timqa/my-rest-api/docs"
	"github.com/timqa/my-rest-api/internal/auth"
	"github.com/timqa/my-rest-api/internal/config"
	"github.com/timqa/my-rest-api/internal/platform/metrics"
	"github.com/timqa/my-rest-api/internal/platform/postgres"
	"github.com/timqa/my-rest-api/internal/server"
	"github.com/timqa/my-rest-api/internal/user"
)

// main — тонкая обёртка: настроить лог, запустить run(),
// и ТОЛЬКО после возврата run() решить про код выхода.
// Здесь и только здесь вызывается os.Exit — когда все defer-ы уже отработали.
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run содержит всю логику запуска и возвращает ошибку вместо os.Exit.
// Благодаря этому все defer-ы (pool.Close, cancel, stop) выполнятся всегда,
// а os.Exit останется ровно один — в main, после возврата run().
func run() error {
	// Загружаем конфиг из переменных окружения (локально — из .env).
	// Возвращаем обёрнутую ошибку с контекстом "где именно упало".
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Context, который отменяется при SIGINT (Ctrl+C) или SIGTERM
	// (docker stop, kubernetes). Перехватывая сигналы, мы отменяем
	// дефолтное "убить процесс немедленно" и получаем шанс на плавную остановку.
	// stop() отменяет регистрацию обработчиков сигналов — освобождает ресурсы.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Подключаемся к Postgres: создаём пул и проверяем живой коннект (fail fast).
	// Если БД недоступна — падаем сразу на старте, а не на первом запросе.
	pool, err := postgres.NewPool(ctx, cfg.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	// Закрываем пул при выходе из run(). Стоит сразу после создания —
	// так его невозможно забыть, даже если ниже добавятся ранние return.
	defer pool.Close()

	// Подтверждаем в логах, что связка с БД установлена.
	slog.Info("connected to postgres", "host", cfg.Postgres.Host, "db", cfg.Postgres.DB)

	// Реестр метрик — один на процесс, создаётся в composition root и
	// передаётся всем, кто должен в него писать или из него читать.
	mreg := metrics.New()

	// Собираем HTTP-сервер: роутер, middleware, роуты, таймауты.
	// CORS-белый список передаём из конфига — сервер сам не знает,
	// какие origin'ы разрешены, это решает приложение (composition root).
	// readyFn — readiness-проверка: ping БД под свободным контекстом.
	// readyz вызывается при живом сервере, поэтому ctx запроса тут
	// не нужен — берём Background с таймаутом (см. readyTimeout в server).
	srv := server.New(cfg.AppPort, cfg.CORS.AllowedOrigins, func(ctx context.Context) error {
		return pool.Ping(ctx)
	}, mreg)

	// --- Composition root: связываем фичи здесь, в одном месте ---
	// Слои фичи user собираем "в столбик": pool -> repository -> service -> handler.
	// Каждый получает зависимость через конструктор (DI), а сам ничего не создаёт.
	userRepo := user.NewRepository(pool)
	userService := user.NewService(userRepo)

	// Менеджер JWT: секрет и время жизни access-токена берём из конфига (env).
	tokenManager := auth.NewTokenManager(cfg.JWT.Secret, cfg.JWT.TTL)

	// Слои фичи auth: repository (хранение refresh) -> service (оркестрация сессии).
	// userService передаём как auth.RoleLookup: auth при refresh спросит у него
	// АКТУАЛЬНУЮ роль. Именно здесь, в main, "мост" через интерфейс и замыкается —
	// поэтому ни auth, ни user не знают друг о друге (цикла импортов нет).
	authRepo := auth.NewRepository(pool)
	authService := auth.NewService(tokenManager, authRepo, userService, cfg.JWT.RefreshTTL)

	// --- Фоновая чистка протухших refresh-токенов ---
	// Без неё таблица refresh_tokens растёт вечно: каждый логин/ротация
	// вставляет строку, а протухшие никогда не удаляются.
	//
	// Горутина живёт под ТЕМ ЖЕ сигнальным ctx, что и сервер: при
	// SIGTERM/SIGINT она услышит отмену и закончится сама — фоновые job-ы
	// не убиваются принудительно, они останавливаются тем же сигналом.
	//
	// WaitGroup — чтобы graceful shutdown ДОЖДАЛСЯ фонового job-а:
	// без него run() мог бы выйти раньше, чем чистка допишет своё,
	// а за ней уже закроется пул БД под её ногами.
	var cleanupWG sync.WaitGroup
	cleanupWG.Add(1)
	go func() {
		defer cleanupWG.Done()
		authService.RunCleanup(ctx, cfg.JWT.CleanupInterval)
	}()

	// user.Handler выпускает пару токенов через authService и проверяет
	// access-токен через tokenManager.
	userHandler := user.NewHandler(userService, authService, tokenManager)
	// auth.Handler обслуживает /auth/refresh, /auth/logout и /auth/logout-all.
	// TokenManager передаём и сюда: logout-all — защищённая ручка,
	// ей нужна проверка access-токена (AuthRequired).
	authHandler := auth.NewHandler(authService, tokenManager)

	// Вешаем роуты фич на группу с версией API. Пути объявляет сама фича,
	// а префикс задаёт приложение. Обе фичи используют подгруппу /auth —
	// gin их смёржит: /api/v1/auth/{register,login,refresh,logout}.
	api := srv.Router().Group("/api/v1")
	userHandler.RegisterRoutes(api)
	authHandler.RegisterRoutes(api)

	// Swagger UI — на /swagger/index.html.
	// Статику UI отдаёт ginSwagger, а спецификацию (spec) регистрирует
	// пакет docs через свой init() при blank-импорте "_ .../docs".
	// Этот роут НЕ под /api/v1 — документация живёт отдельно от API.
	srv.Router().GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Запускаем сервер в отдельной горутине, чтобы run() мог ждать сигнал.
	// Буферизованный канал (ёмкость 1) — чтобы горутина не "зависла" на отправке,
	// если run() уже вышел из select. Защита от утечки горутины.
	errCh := make(chan error, 1)
	go func() {
		// Run блокируется, пока сервер слушает, и вернёт ошибку только если
		// сервер не смог стартовать (например, пор занят).
		if err := srv.Run(); err != nil {
			errCh <- err
		}
	}()

	// Блокируемся на одном из двух событий:
	// 1) сервер упал с ошибкой, 2) пришёл сигнал остановки.
	select {
	case err := <-errCh:
		// Сервер не смог работать — возвращаем ошибку, defer-ы закроют пул.
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		// Пришёл SIGINT/SIGTERM — идём в плавную остановку.
		slog.Info("shutdown signal received, stopping...")
	}

	// Свежий контекст с дедлайном 10с на плавную остановку.
	// ВАЖНО: берём его от context.Background(), а НЕ от ctx — тот уже отменён
	// сигналом, и Shutdown с ним не дал бы серверу доработать запросы.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown перестаёт принимать новые соединения и ждёт завершения
	// уже активных запросов, пока не истечёт shutdownCtx.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// Дожидаемся фонового job-а чистки: он должен успеть завершить текущий
	// DELETE, прежде чем defer pool.Close() заберёт у него соединения.
	// Порядок "сервер → фоновые job-ы → пул" — правильный graceful shutdown.
	cleanupWG.Wait()

	// Дошли сюда — сервер остановлен корректно. Ниже сработают defer-ы
	// (cancel → pool.Close → stop), затем run() вернёт nil, и main завершится с кодом 0.
	slog.Info("server stopped gracefully")
	return nil
}
