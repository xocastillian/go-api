package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// refreshTokenBytes — длина случайной части refresh-токена.
// 32 байта = 256 бит энтропии: прямой перебор невозможен физически.
const refreshTokenBytes = 32

// generateRefreshToken создаёт новый криптостойкий случайный токен.
//
// Почему crypto/rand, а не math/rand: math/rand детерминирован —
// по нескольким значениям можно восстановить последовательность и
// предсказать следующие токены. Для секретов это недопустимо; crypto/rand
// берёт энтропию из ОС и непредсказуем.
func generateRefreshToken() (string, error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}

	// base64.RawURLEncoding — алфавит безопасен для URL и JSON,
	// без выравнивающих символов "=" в конце. Токен можно положить
	// и в тело, и в cookie, и в заголовок без экранирования.
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashRefreshToken считает sha256-хеш токена — именно его кладём в БД.
//
// В БД НИКОГДА не храним сам токен (как и пароль): утечка базы не должна
// отдавать злоумышленнику рабочие токены. Из sha256 восстановить оригинал
// нельзя, а сверить — можно: клиент присылает токен, мы хешируем и ищем запись.
//
// ВАЖНО: здесь sha256, а НЕ bcrypt. Причина — энтропия. Пароль предсказуем
// (люди выбирают слабые), поэтому нужен медленный bcrypt со стоимостью.
// У refresh-токена 256 бит случайности — перебор и так невозможен, а медленный
// хеш лишь тормозил бы каждый /refresh без всякой пользы.
func hashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
