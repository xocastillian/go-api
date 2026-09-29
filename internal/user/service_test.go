package user

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/timqa/my-rest-api/internal/apperr"
)

// =====================================================================
// Фейк хранилища пользователей.
//
// Реализует интерфейс userStore (см. service.go). Помимо управляемых
// ответов, ЗАПОМИНАЕТ аргументы вызовов — это нужно, чтобы проверить,
// ЧТО ИМЕННО сервис передал вниз (нормализованный email, хеш пароля).
// =====================================================================

type fakeUserStore struct {
	// Create
	createResult User
	createErr    error
	createArg    User // что пришло на вход

	// GetByEmail
	getByEmailResult User
	getByEmailErr    error
	getByEmailArg    string

	// GetByID
	getByIDResult User
	getByIDErr    error

	// UpdatePassword
	updatePasswordErr  error
	updatePasswordID   uuid.UUID // uuid.Nil = метод НЕ звали
	updatePasswordHash string

	// Delete
	deleteErr error
	deleteID  uuid.UUID

	// List
	listResult []User
	listErr    error
}

func (f *fakeUserStore) Create(_ context.Context, u User) (User, error) {
	f.createArg = u
	return f.createResult, f.createErr
}

func (f *fakeUserStore) GetByEmail(_ context.Context, email string) (User, error) {
	f.getByEmailArg = email
	return f.getByEmailResult, f.getByEmailErr
}

func (f *fakeUserStore) GetByID(_ context.Context, _ uuid.UUID) (User, error) {
	return f.getByIDResult, f.getByIDErr
}

func (f *fakeUserStore) UpdatePassword(_ context.Context, id uuid.UUID, hash string) error {
	f.updatePasswordID = id
	f.updatePasswordHash = hash
	return f.updatePasswordErr
}

func (f *fakeUserStore) Delete(_ context.Context, id uuid.UUID) error {
	f.deleteID = id
	return f.deleteErr
}

func (f *fakeUserStore) List(_ context.Context, _ int, _ int) ([]User, error) {
	return f.listResult, f.listErr
}

// mustHash — помощник: хеширует пароль для подготовки тестовых данных
// (например, чтобы изобразить юзера с уже сохранённым паролем).
func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("хеширование пароля: %v", err)
	}
	return string(hash)
}

// =====================================================================
// Register
// =====================================================================

func TestService_Register_NormalizesEmailAndHashesPassword(t *testing.T) {
	store := &fakeUserStore{
		createResult: User{ID: uuid.New(), Email: "a@b.com", Role: RoleUser},
	}
	svc := NewService(store)

	// Подаём email с пробелами и верхним регистром — сервис обязан привести к канону.
	_, err := svc.Register(context.Background(), "  A@B.COM  ", "sekret123")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if store.createArg.Email != "a@b.com" {
		t.Errorf("email не нормализован: получили %q", store.createArg.Email)
	}
	if store.createArg.Role != RoleUser {
		t.Errorf("роль: получили %q, ждали %q", store.createArg.Role, RoleUser)
	}
	// Пароль НЕ должен уйти в репозиторий в открытом виде.
	if store.createArg.PasswordHash == "sekret123" {
		t.Errorf("пароль сохранён открытым текстом!")
	}
	// А хеш должен соответствовать исходному паролю.
	if err := bcrypt.CompareHashAndPassword([]byte(store.createArg.PasswordHash), []byte("sekret123")); err != nil {
		t.Errorf("хеш не соответствует паролю: %v", err)
	}
}

func TestService_Register_PropagatesEmailTaken(t *testing.T) {
	store := &fakeUserStore{createErr: apperr.ErrEmailTaken}
	svc := NewService(store)

	_, err := svc.Register(context.Background(), "x@y.com", "sekret123")
	if !errors.Is(err, apperr.ErrEmailTaken) {
		t.Fatalf("получили %v, ждали ErrEmailTaken", err)
	}
}

// =====================================================================
// Login
// =====================================================================

func TestService_Login_Success(t *testing.T) {
	want := User{
		ID:           uuid.New(),
		Email:        "a@b.com",
		PasswordHash: mustHash(t, "sekret123"),
		Role:         RoleUser,
	}
	store := &fakeUserStore{getByEmailResult: want}
	svc := NewService(store)

	// Логинимся "грязным" email — сервис должен нормализовать и найти юзера.
	got, err := svc.Login(context.Background(), "A@B.com", "sekret123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("вернулся не тот пользователь: %v", got.ID)
	}
	if store.getByEmailArg != "a@b.com" {
		t.Errorf("email не нормализован перед поиском: %q", store.getByEmailArg)
	}
}

func TestService_Login_WrongPassword(t *testing.T) {
	store := &fakeUserStore{
		getByEmailResult: User{ID: uuid.New(), PasswordHash: mustHash(t, "correct-password")},
	}
	svc := NewService(store)

	_, err := svc.Login(context.Background(), "a@b.com", "wrong-password")
	if !errors.Is(err, apperr.ErrInvalidCredentials) {
		t.Fatalf("получили %v, ждали ErrInvalidCredentials", err)
	}
}

func TestService_Login_UnknownEmail_HidesExistence(t *testing.T) {
	// Хранилище говорит "не найдено", но НАРУЖУ это протекать не должно:
	// клиент не должен отличать "нет такого email" от "неверный пароль".
	store := &fakeUserStore{getByEmailErr: apperr.ErrNotFound}
	svc := NewService(store)

	_, err := svc.Login(context.Background(), "nobody@b.com", "whatever")
	if !errors.Is(err, apperr.ErrInvalidCredentials) {
		t.Fatalf("получили %v, ждали ErrInvalidCredentials (а НЕ ErrNotFound)", err)
	}
}

// =====================================================================
// ChangePassword
// =====================================================================

func TestService_ChangePassword_WrongOldPassword(t *testing.T) {
	id := uuid.New()
	store := &fakeUserStore{
		getByIDResult: User{ID: id, PasswordHash: mustHash(t, "old-password")},
	}
	svc := NewService(store)

	err := svc.ChangePassword(context.Background(), id, "WRONG-old", "new-password")
	if !errors.Is(err, apperr.ErrInvalidCredentials) {
		t.Fatalf("получили %v, ждали ErrInvalidCredentials", err)
	}
	// КЛЮЧЕВОЕ: при неверном старом пароле запись НЕ должна произойти.
	if store.updatePasswordID != uuid.Nil {
		t.Errorf("UpdatePassword не должен вызываться при неверном старом пароле")
	}
}

func TestService_ChangePassword_Success(t *testing.T) {
	id := uuid.New()
	store := &fakeUserStore{
		getByIDResult: User{ID: id, PasswordHash: mustHash(t, "old-password")},
	}
	svc := NewService(store)

	err := svc.ChangePassword(context.Background(), id, "old-password", "new-password")
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	if store.updatePasswordID != id {
		t.Fatalf("UpdatePassword вызван не для того id: %v", store.updatePasswordID)
	}
	// Сохранённый хеш должен соответствовать НОВОМУ паролю.
	if err := bcrypt.CompareHashAndPassword([]byte(store.updatePasswordHash), []byte("new-password")); err != nil {
		t.Errorf("хеш не соответствует новому паролю: %v", err)
	}
}
