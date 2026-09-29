package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/timqa/my-rest-api/internal/apperr"
)

// RoleLookup — минимальный контракт, который auth нужно от "пользователей":
// узнать актуальную роль по id, чтобы выпустить новый access-токен при refresh.
//
// ПОЧЕМУ интерфейс объявлен ЗДЕСЬ, а не в пакете user:
// user импортирует auth (нужен TokenManager), поэтому обратный импорт auth→user
// дал бы ЦИКЛ. Выход — инверсия зависимостей: потребитель (auth) сам описывает,
// что ему нужно, а конкретную реализацию подставит composition root (main.go).
// Пакет user при этом про auth не знает вообще.
type RoleLookup interface {
	RoleByID(ctx context.Context, id uuid.UUID) (string, error)
}

// sessionStore — контракт, который auth.Service ждёт от "хранилища сессий".
// Описывает только те методы, что сервис реально вызывает. Объявлен у
// потребителя (service): так его можно подменить фейком в юнит-тестах,
// а *Repository подходит структурно — менять его не нужно.
//
// Возвращает НЕэкспортируемый refreshRecord — это нормально: интерфейс
// объявлен в том же пакете, а наружу тип не протекает (наружу торчит сервис).
type sessionStore interface {
	Create(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error
	GetByHash(ctx context.Context, tokenHash string) (refreshRecord, error)

	// GetByHashForUpdate — то же, но с блокировкой строки (FOR UPDATE):
	// конкурентный Rotate на том же токене подождёт коммита первого и
	// увидит уже отозванный токен — детект кражи сработает честно, а не
	// "из одного токена две сессии".
	GetByHashForUpdate(ctx context.Context, tokenHash string) (refreshRecord, error)

	Revoke(ctx context.Context, id uuid.UUID) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error

	// DeleteExpired — один проход чистки: убрать протухшие, вернуть сколько.
	// Данные для фоновой чистки (cleanup.go); период задаёт вызывающий.
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)

	// WithTx — "выполни fn атомарно, в одной транзакции БД".
	// fn получает store, работающий ВНУТРИ транзакции; nil из fn → COMMIT,
	// ошибка → ROLLBACK. Нужен Rotate: "отозвать старый + создать новый" —
	// два зависимых шага, которые обязаны исполниться вместе.
	WithTx(ctx context.Context, fn func(sessionStore) error) error
}

// Service — оркестрация жизненного цикла сессии:
// выдать пару (логин) → обновить с ротацией (refresh) → отозвать (logout).
type Service struct {
	tokens     *TokenManager
	repo       sessionStore
	roles      RoleLookup
	refreshTTL time.Duration
}

// NewService — конструктор. Все зависимости и срок жизни приходят снаружи (DI).
// Хранилище: в бою *Repository, в тестах — фейк.
func NewService(tokens *TokenManager, repo sessionStore, roles RoleLookup, refreshTTL time.Duration) *Service {
	return &Service{tokens: tokens, repo: repo, roles: roles, refreshTTL: refreshTTL}
}

// IssuePair выпускает access + refresh и сохраняет refresh в БД.
// Вызывается при логине и регистрации.
func (s *Service) IssuePair(ctx context.Context, userID uuid.UUID, role string) (TokenPair, error) {
	// Обычный путь: сохранение через s.repo, каждый INSERT сам по себе.
	return s.issuePairWith(ctx, s.repo, userID, role)
}

// issuePairWith — общая "внутренность" выпуска пары, параметризованная
// ХРАНИЛИЩЕМ, через которое сохраняется refresh.
//
// Зачем параметр: IssuePair сохраняет через s.repo (пул — INSERT сам по
// себе, транзакция не нужна), а Rotate — через tx-store ВНУТРИ транзакции,
// чтобы Create нового refresh был атомарен с Revoke старого. Логика одна,
// различается только "куда писать".
func (s *Service) issuePairWith(ctx context.Context, store sessionStore, userID uuid.UUID, role string) (TokenPair, error) {
	// Access — обычный JWT (stateless), на сервере не хранится.
	access, err := s.tokens.Issue(userID, role)
	if err != nil {
		return TokenPair{}, err
	}

	// Refresh — случайная строка; в БД кладём только её хеш.
	refresh, err := generateRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}

	expiresAt := time.Now().Add(s.refreshTTL)
	if err := store.Create(ctx, userID, hashRefreshToken(refresh), expiresAt); err != nil {
		return TokenPair{}, err
	}

	// Клиенту отдаём СЫРОЙ refresh (только он его знает), в БД лежит хеш.
	return TokenPair{AccessToken: access, RefreshToken: refresh}, nil
}

// Rotate обменивает валидный refresh-токен на НОВУЮ пару.
// Старый refresh при этом отзывается (ротация). Повторное использование
// отозванного токена трактуется как кража.
//
// ВЕСЬ обмен идёт в ОДНОЙ транзакции БД. Почему это критично: ротация —
// два зависимых шага ("старый отозвать", "новый создать"). Без транзакции
// сбой посередине оставлял бы клиента НАВСЕГДА разлогиненным: старый токен
// уже мёртв, а нового не существует — повторить операцию нечем. Транзакция
// делает операцию неделимой: упало — откатилось всё, клиент просто повторит.
func (s *Service) Rotate(ctx context.Context, refreshToken string) (TokenPair, error) {
	// outErr — итог для клиента. Он МОЖЕТ быть не-nil при ЗАКОММИЧЕННОЙ
	// транзакции: в кейсе кражи мы обязаны СОХРАНИТЬ отзыв всех сессий,
	// хотя клиенту возвращаем ошибку. Поэтому "ошибка из fn → rollback"
	// и "итог для клиента" разделены (см. комментарии ниже).
	var (
		pair   TokenPair
		outErr error
	)

	// WithTx: fn вернул nil → COMMIT, ошибку → ROLLBACK.
	err := s.repo.WithTx(ctx, func(store sessionStore) error {
		// Ищем по ХЕШУ (клиент прислал сырой токен) — и с БЛОКИРОВКОЙ
		// строки (FOR UPDATE): конкурентный refresh тем же токеном
		// подождёт здесь до конца нашей транзакции и увидит её итог,
		// а не "протухший" снимок до отзыва.
		rec, err := store.GetByHashForUpdate(ctx, hashRefreshToken(refreshToken))
		if err != nil {
			// Нет записи → токен неизвестен/подделан. В БД мы ещё ничего
			// не писали, поэтому коммит пустой транзакции безвреден.
			if errors.Is(err, apperr.ErrNotFound) {
				outErr = ErrInvalidToken
				return nil
			}
			return err // сбой БД → откат
		}

		// ДЕТЕКТ КРАЖИ: токен уже отозван, а им снова пользуются.
		// Легитимный клиент после ротации держит НОВЫЙ токен, старый — не
		// трогает. Значит, старый экземпляр утёк. Отзываем ВСЕ сессии
		// пользователя: лучше разлогинить и вора, и владельца, чем оставить
		// доступ вору.
		if rec.RevokedAt != nil {
			if err := store.RevokeAllForUser(ctx, rec.UserID); err != nil {
				return err // сбой БД → откат (отзыв тоже не применится)
			}
			// КЛЮЧЕВОЙ момент атомарности: отзыв всех сессий ДОЛЖЕН
			// сохраниться, хотя клиент получит ошибку. Поэтому из fn
			// возвращаем nil (→ COMMIT), а ErrInvalidToken отдаём через
			// outErr. Верни мы ошибку из fn — WithTx откатил бы и отзыв.
			outErr = ErrInvalidToken
			return nil
		}

		// Истёк срок — отказываем. Писать в БД нечего, коммит пустой.
		if time.Now().After(rec.ExpiresAt) {
			outErr = ErrInvalidToken
			return nil
		}

		// Свежая роль из БД (не из старого токена): за время жизни refresh
		// пользователя могли повысить/понизить. Интерфейс RoleLookup
		// позволяет сделать это без импорта пакета user.
		// (Чтение идёт мимо транзакции — это read-only справочник; если
		// юзера удалят прямо сейчас, ниже всё равно отработает ErrNotFound.)
		role, err := s.roles.RoleByID(ctx, rec.UserID)
		if err != nil {
			// Пользователя удалили — трактуем как невалидный токен, не как 500.
			if errors.Is(err, apperr.ErrNotFound) {
				outErr = ErrInvalidToken
				return nil
			}
			return err
		}

		// Ротация, шаг 1: помечаем старый отозванным.
		if err := store.Revoke(ctx, rec.ID); err != nil {
			return err
		}

		// Ротация, шаг 2: выпускаем новую пару ВНУТРИ ТОЙ ЖЕ транзакции.
		// Если Create нового refresh упадёт — Revoke старого откатится
		// ВМЕСТЕ с ним, и клиент сможет просто повторить запрос.
		pair, err = s.issuePairWith(ctx, store, rec.UserID, role)
		return err
	})

	// err != nil — сбой самой транзакции (begin/commit) или ошибка из fn:
	// всё откатилось, ничего не применилось. outErr != nil при err == nil —
	// "бизнес-отказ" (кража/протух/нет записи), транзакция закоммичена.
	if err != nil {
		return TokenPair{}, err
	}
	return pair, outErr
}

// Revoke отзывает refresh-токен (logout).
// Идемпотентен: неизвестный/уже отозванный токен — НЕ ошибка,
// logout можно повторять сколько угодно.
func (s *Service) Revoke(ctx context.Context, refreshToken string) error {
	rec, err := s.repo.GetByHash(ctx, hashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.repo.Revoke(ctx, rec.ID)
}

// RevokeAllForUser отзывает ВСЕ активные refresh-сессии пользователя разом.
//
// Зачем нужен отдельный метод:
//   - при смене пароля — пароль мог утечь, и все ранее выданные refresh-токены
//     (возможно, уже у злоумышленника) должны умереть. Меняя пароль, владелец
//     ожидает, что "все остальные устройства" разлогинятся;
//   - это же готовый инструмент для "выход со всех устройств" в будущем.
//
// Возвращает ошибку ТОЛЬКО при сбое БД. Ситуация "нет активных сессий" —
// это НЕ ошибка (как и в Revoke): отзывать нечего, цель achieved.
//
// Метод — тонкая обёртка над одноимённым методом репозитория. Живёт здесь,
// а не вызывается напрямую из handler'а, чтобы наружу (за пределы пакета auth)
// торчал только СЕРВИС, а не репозиторий: внешние слои не должны знать,
// что сессии вообще где-то в БД хранятся.
func (s *Service) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	return s.repo.RevokeAllForUser(ctx, userID)
}
