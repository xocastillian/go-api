// cleanup.go — фоновая чистка протухших refresh-токенов.
//
// Зачем нужен файл: чистка — не бизнес-ручка и не запрос, это ДОЛГОЖИВУЩИЙ
// фоновый процесс с собственным жизненным циклом (тикер, остановка по
// сигналу). Собирать его отдельно от service.go — та же раскладка "один
// файл = одна обязанность", что domain.go / token.go / repository.go.
//
// Кто владеет горутиной: НЕ этот метод. RunCleanup блокируется до отмены
// ctx; запускать его должен composition root (main.go) — там же, где
// поднимается HTTP-сервер, и под тем же сигнальным контекстом.
package auth

import (
	"context"
	"log/slog"
	"time"
)

// CleanupExpired — ОДИН проход чистки: удалить протухшие refresh-токены,
// вернуть число убранных. Ошибку отдаёт наверх (логирует RunCleanup).
// Тонкий метод: правила "что удалять" живут в репозитории (DeleteExpired).
func (s *Service) CleanupExpired(ctx context.Context) (int64, error) {
	return s.repo.DeleteExpired(ctx, time.Now())
}

// RunCleanup — фоновый цикл чистки: раз в interval зовёт CleanupExpired.
// Блокируется до отмены ctx — запускать в отдельной горутине (main.go).
func (s *Service) RunCleanup(ctx context.Context, interval time.Duration) {
	// Первую чистку делаем СРАЗУ при старте, не через interval:
	// сервис мог быть выключен долго, и к его подъёму накопился стухший
	// мусор. Тикер же первый тик даст только ЧЕРЕЗ interval.
	if _, err := s.CleanupExpired(ctx); err != nil {
		// Фоновая задача НЕ должна валить процесс: логируем и продолжаем —
		// следующий тик попробует снова. Это общее правило для джобов:
		// ошибка тика ≠ ошибка сервиса.
		slog.Error("refresh tokens cleanup failed", "err", err)
	}

	// Ticker — таймер, тикающий раз в interval. defer Stop обязателен:
	// иначе тикер держит системный ресурс до конца процесса.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// select ждёт ОДНОГО из двух событий: сигнал остановки или тик.
		select {
		case <-ctx.Done():
			// SIGINT/SIGTERM — цикл завершается, горутина умирает штатно.
			// Именно так фоновые job-ы встраиваются в graceful shutdown:
			// они не убиваются принудительно, а слышат тот же сигнал,
			// что и сервер, и заканчиваются сами.
			slog.Info("refresh tokens cleanup stopped")
			return

		case <-ticker.C:
			// ПОДВОХ: ctx к этому моменту ЖИВОЙ (иначе мы бы уже вышли),
			// но он может быть отменён В СЕРЕДИНЕ чистки — и тогда запрос
			// к БД оборвётся с шумной ошибкой "context canceled" в логах
			// при КАЖДОМ останове. Нужен контекст, наследующий значения
			// (метаданные), но НЕ отмену — context.WithoutCancel.
			// Отдельный таймаут не нужен: DELETE по индексу — быстрый запрос.
			cleanCtx := context.WithoutCancel(ctx)

			deleted, err := s.CleanupExpired(cleanCtx)
			switch {
			case err != nil:
				// См. выше: фоновая задача не вали процесс.
				slog.Error("refresh tokens cleanup failed", "err", err)
			case deleted > 0:
				// Логируем ТОЛЬКО когда что-то убрали: тик раз в час с
				// "deleted=0" — чистый шум в логах.
				slog.Info("refresh tokens cleaned", "deleted", deleted)
			}
		}
	}
}
