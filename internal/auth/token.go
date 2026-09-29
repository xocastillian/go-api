// Package auth отвечает за аутентификацию: выпуск и проверку JWT-токенов.
//
// Почему это отдельный пакет, а не часть user:
// токены нужны не только пользователям — завтра защищённые ручки заметок
// тоже потребуют проверки токена. auth — сквозная (cross-cutting) фича,
// её переиспользуют все остальные. При этом auth НЕ импортирует user:
// он работает с общими данными (id, роль), а не с доменной моделью User.
// Поэтому цикла зависимостей не возникает.
package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TokenManager выпускает и разбирает JWT-токены.
// Он хранит секрет подписи и время жизни токена — оба приходят снаружи (DI),
// а не читаются из глобального конфига: так пакет независим от config,
// а в тестах можно подставить свой секрет и короткий TTL.
type TokenManager struct {
	secret []byte        // ключ подписи/проверки (из env, НЕ хардкод)
	ttl    time.Duration // сколько живёт выпущенный токен
}

// NewTokenManager — конструктор. Получает секрет и TTL (dependency injection).
func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	return &TokenManager{secret: []byte(secret), ttl: ttl}
}

// jwtClaims — внутреннее представление payload в терминах библиотеки.
// Встраиваем RegisteredClaims, чтобы получить стандартные поля:
// sub (subject — субъект), iat (issued at — выдан), exp (expires at — истекает).
// Их понимает любой JWT-клиент, а exp библиотека проверяет автоматически.
type jwtClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// Issue создаёт подписанный токен для пользователя и возвращает его строкой.
func (m *TokenManager) Issue(userID uuid.UUID, role string) (string, error) {
	now := time.Now()
	claims := jwtClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),                    // кто это (id пользователя)
			IssuedAt:  jwt.NewNumericDate(now),            // когда выдан
			ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)), // когда протухнет
		},
	}

	// JWT = header.payload.signature. Заголовок и payload НЕ шифруются —
	// их может прочитать любой. Поэтому в токен нельзя класть пароли/секреты:
	// подпись гарантирует лишь ЦЕЛОСТНОСТЬ (что токен не подделали), не secrecy.
	//
	// HS256 — симметричная подпись: одной и той же секретной строкой и
	// подписываем, и проверяем. Значит секрет есть только у сервера.
	// (Альтернатива — RS256, асимметричная: приватным ключом подписываем,
	// публичным проверяем. Нужна, когда проверяющих сервисов много.)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// SignedString считает подпись по секрету и склеивает три части в строку.
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// Parse проверяет подпись и срок действия токена и возвращает его claims.
func (m *TokenManager) Parse(tokenString string) (Claims, error) {
	claims := &jwtClaims{}

	// keyFunc библиотека вызывает, чтобы получить ключ проверки подписи.
	keyFunc := func(t *jwt.Token) (any, error) {
		// КРИТИЧНО для безопасности: убеждаемся, что алгоритм токена — HMAC.
		// Без этой проверки возможна "alg confusion" атака: злоумышленник
		// подсовывает токен с другим alg (например, "none" или RS256),
		// и проверка подписи обходится. Всегда проверяй метод явно.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	}

	// ParseWithClaims сам проверит exp (срок) и валидность подписи.
	// Если токен просрочен/подделан/битый — вернёт err.
	token, err := jwt.ParseWithClaims(tokenString, claims, keyFunc)
	if err != nil || !token.Valid {
		return Claims{}, fmt.Errorf("parse token: %w", ErrInvalidToken)
	}

	// Subject у нас хранит строковое представление UUID.
	// Парсим обратно в uuid.UUID — заодно отсекаем мусор.
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Claims{}, fmt.Errorf("parse subject: %w", ErrInvalidToken)
	}

	return Claims{UserID: userID, Role: claims.Role}, nil
}
