// Package apperr — общий словарь доменных ошибок приложения.
//
// Зачем отдельный пакет, а не ошибки внутри каждой фичи:
// ошибку создаёт один слой (например, user.Repository), а проверяет
// другой (user.Service или httputil). Если бы ошибки лежали в пакете user,
// то httputil пришлось бы импортировать user. Общий маленький пакет apperr
// разрывает эту связь: и фичи, и инфраструктура зависят только от него,
// и циклов импортов не возникает.
package apperr

import "errors"

// Сентинельные ошибки — "эталонные значения", с которыми сравниваются
// другие через errors.Is. Сравнение идёт по идентичности значения, а не по
// тексту, поэтому опечатка или смена текста ошибки не сломают логику.
var (
	// ErrNotFound — запрошенная сущность не существует.
	ErrNotFound = errors.New("not found")

	// ErrEmailTaken — email уже зарегистрирован (нарушение UNIQUE).
	ErrEmailTaken = errors.New("email already taken")

	// ErrInvalidCredentials — неверная пара email/пароль при логине.
	ErrInvalidCredentials = errors.New("invalid credentials")
)
