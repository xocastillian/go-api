package auth

import (
	"errors"

	"github.com/google/uuid"
)

// Этот файл собирает ДОМЕННЫЕ типы фичи auth: значения и понятия
// предметной области, которые не зависят ни от JWT-библиотеки, ни от gin,
// ни от pgx. Аналогично user/domain.go — так раскладка единообразна:
// домен — в domain.go, инфраструктура — в token.go/repository.go,
// оркестрация — в service.go.

// ErrInvalidToken — токен отсутствует, просрочен, подделан или повреждён.
// Единственная ошибка авторизации, которую наружу видит HTTP-слой.
// Держим её здесь, а не в apperr: она нужна ровно в этом пакете
// (token.go и middleware.go) и как доменное понятие наружу не протекает.
var ErrInvalidToken = errors.New("invalid token")

// Claims — то, что мы кладём в токен и достаём из него обратно.
// Намеренно минимум: кто это (UserID) и что ему можно (Role).
// Ничего чувствительного здесь быть не должно — см. комментарий в Issue.
type Claims struct {
	UserID uuid.UUID
	Role   string
}

// TokenPair — пара токенов, которую отдаём клиенту после логина/refresh.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
}
