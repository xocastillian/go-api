package user

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/timqa/my-rest-api/internal/apperr"
)

// userStore — контракт, который user.Service ждёт от "хранилища пользователей".
// Описывает ТОЛЬКО те методы, что сервис реально вызывает.
//
// Зачем интерфейс, а не конкретный *Repository:
//   - юнит-тесты подсовывают сюда фейк (реализацию в памяти). С конкретным
//     типом это невозможно — компилятор потребует именно *Repository,
//     а он ходит в настоящую Postgres.
//   - *Repository удовлетворяет интерфейсу СТРУКТУРНО (совпадение сигнатур
//     методов), поэтому сам репозиторий менять НЕ нужно.
//
// Интерфейс объявлен у ПОТРЕБИТЕЛЯ (service), а не рядом с реализацией:
// так он описывает ровно то, что сервису нужно, и не жирнеет лишним.
type userStore interface {
	Create(ctx context.Context, u User) (User, error)
	GetByEmail(ctx context.Context, email string) (User, error)
	GetByID(ctx context.Context, id uuid.UUID) (User, error)
	UpdatePassword(ctx context.Context, id uuid.UUID, passwordHash string) error
	Delete(ctx context.Context, id uuid.UUID) error
	List(ctx context.Context, limit, offset int) ([]User, error)
}

// Service — бизнес-логика, связанная с пользователями.
// Держит хранилище как зависимость и оркестрирует работу с ним.
type Service struct {
	users userStore
}

// NewService — конструктор. Хранилище передаётся снаружи (DI):
// в бою это *Repository, в тестах — фейк.
func NewService(users userStore) *Service {
	return &Service{users: users}
}

// Register регистрирует нового пользователя: хеширует пароль и сохраняет.
func (s *Service) Register(ctx context.Context, email, password string) (User, error) {
	// Приводим email к каноническому виду ДО сохранения: иначе "A@B.com"
	// и "a@b.com" лягут двумя разными строками и обойдут UNIQUE.
	email = normalizeEmail(email)

	// bcrypt.GenerateFromPassword хеширует пароль.
	// Второй аргумент — "стоимость" (cost): сколько раз прогонять алгоритм.
	// DefaultCost (10) — разумный баланс безопасность/скорость.
	// Хранить можно ТОЛЬКО хеш, восстановить пароль из него нельзя.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	// Собираем доменную модель. Роль по умолчанию — user.
	u := User{
		Email:        email,
		PasswordHash: string(hash),
		Role:         RoleUser,
	}

	// Отдаём в репозиторий. Он вернёт созданного юзера (с id, timestamps)
	// либо уже доменную ошибку (apperr.ErrEmailTaken и т.п.).
	created, err := s.users.Create(ctx, u)
	if err != nil {
		return User{}, err
	}
	return created, nil
}

// Login проверяет email+пароль и возвращает пользователя при успехе.
func (s *Service) Login(ctx context.Context, email, password string) (User, error) {
	// То же правило нормализации, что и в Register — обязательно.
	// Иначе зарегистрировался как a@b.com, а залогиниться попытаешься
	// как A@b.com и не найдёшься. Оба входа обязаны нормализовать одинаково.
	email = normalizeEmail(email)

	// Ищем пользователя по email.
	u, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		// Если не нашли — НЕ говорим клиенту "такого email нет".
		// Отдаём нейтральную ошибку, чтобы не палить существующие адреса.
		if errors.Is(err, apperr.ErrNotFound) {
			return User{}, apperr.ErrInvalidCredentials
		}
		return User{}, err
	}

	// bcrypt.CompareHashAndPassword сравнивает сырой пароль с хешем из БД.
	// Возвращает nil при совпадении, иначе ошибку. Сам пароль не расшифровывает.
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return User{}, apperr.ErrInvalidCredentials
	}

	return u, nil
}

// ChangePassword меняет пароль пользователя по его id.
//
// Здесь живёт ТОЛЬКО бизнес-логика самих "паролей": убедиться, что клиент
// знает старый пароль, и записать хеш нового. Отзыв сессий — НЕ здесь:
// это забота фичи auth, и оркестрирует её handler (см. handler.ChangePassword).
// Так user.Service остаётся про пользователей, а не про токены, и нам не
// нужна зависимость user→auth (которая дала бы цикл при сборке).
func (s *Service) ChangePassword(ctx context.Context, id uuid.UUID, oldPassword, newPassword string) error {
	// Загружаем текущего пользователя: нужен его сохранённый хеш,
	// чтобы сравнить со старым паролем. Из токена хеш не достать.
	u, err := s.users.GetByID(ctx, id)
	if err != nil {
		return err
	}

	// Проверяем СТАРЫЙ пароль. Это защита даже при утёкшем access-токене:
	// без знания текущего пароля сменить его не выйдет. Ошибку отдаём той же
	// нейтральной apperr.ErrInvalidCredentials (→ 401), что и в Login —
	// не палим, что именно "неверный старый пароль", а не что-то иное.
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPassword)); err != nil {
		return apperr.ErrInvalidCredentials
	}

	// Хешируем новый пароль. bcrypt сам генерирует свежую соль, поэтому хеш
	// нового пароля никогда не совпадёт с хешем старого, даже если пароли равны.
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	// Пишем новый хеш. Отзыв сессий сделает handler
	return s.users.UpdatePassword(ctx, id, string(hash))
}

// Delete удаляет пользователя вместе со всеми его данными.
//
// Метод тонкий (просто проксирует репозиторий), но живёт в сервисе по делу:
// handler общается ТОЛЬКО с сервисом и не подозревает о репозитории. Когда
// перед удалением появится логика (например, запрет удалять последнего
// админа, отправка письма-подтверждения, запись в аудит) — она ляжет сюда,
// а HTTP-слой не изменится.
//
// Отдельно отзывать сессии здесь не нужно: юзер удаляется ФИЗИЧЕСКИ, и все
// его refresh-токены уходят каскадом на уровне БД (ON DELETE CASCADE).
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.users.Delete(ctx, id)
}

// GetByID возвращает профиль пользователя по id.
// Нужен для защищённой ручки /me: id берётся из токена,
// а актуальные данные — из БД.
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (User, error) {
	return s.users.GetByID(ctx, id)
}

// RoleByID возвращает роль пользователя по id.
// Нужен auth-сервису при refresh: выпуская новый access-токен, он хочет
// взять АКТУАЛЬНУЮ роль (за время жизни refresh её могли изменить).
//
// Этот метод делает user.Service неявным исполнителем интерфейса
// auth.RoleLookup — Go сверяет интерфейсы СТРУКТУРНО, по сигнатуре,
// поэтому импорт auth на стороне user не требуется и цикла не возникает.
func (s *Service) RoleByID(ctx context.Context, id uuid.UUID) (string, error) {
	u, err := s.users.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return string(u.Role), nil
}

// List возвращает страницу пользователей (limit штук, пропустив offset).
// Сейчас метод просто проксирует вызов репозитория, но существует он здесь
// по делу: handler общается ТОЛЬКО с сервисом и не подозревает о репозитории.
// Когда перед выдачей списка появится логика (фильтры, права, кеш),
// она ляжет сюда — а HTTP-слой не изменится.
func (s *Service) List(ctx context.Context, limit, offset int) ([]User, error) {
	return s.users.List(ctx, limit, offset)
}

// normalizeEmail приводит email к каноническому виду, чтобы "A@B.com",
// " a@b.com " и "a@b.com" считались ОДНИМ адресом.
//
// Это доменное правило (одно и то же для регистрации и логина), поэтому
// живёт одной функцией, а не дублируется строкой в двух местах. Вынесено
// рядом с сервисом: репозиторий про правила не знает, handler — тоже.
func normalizeEmail(email string) string {
	// TrimSpace — убираем случайные пробелы по краям (копипаст).
	// ToLower — регистр: домен регистронезависим всегда, а практически
	// и весь адрес целиком (Gmail и т.д.), поэтому лоуэркасим целиком.
	return strings.ToLower(strings.TrimSpace(email))
}
