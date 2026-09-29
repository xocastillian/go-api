package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TestTokenManager_IssueParse_RoundTrip проверяет "счастливый путь":
// токен, выпущенный Issue, читается обратно Parse, а claims совпадают.
// Имя теста по конвенции: Test<Что><_><Сценарий>.
func TestTokenManager_IssueParse_RoundTrip(t *testing.T) {
	// Свежий менеджер с известным секретом и большим TTL — токен не протухнет.
	tm := NewTokenManager("test-secret", time.Hour)
	id := uuid.New()

	token, err := tm.Issue(id, "admin")
	if err != nil {
		// Fatalf = "сломалось так, что дальше нет смысла" — останавливает тест.
		t.Fatalf("Issue вернул ошибку: %v", err)
	}

	claims, err := tm.Parse(token)
	if err != nil {
		t.Fatalf("Parse вернул ошибку на валидном токене: %v", err)
	}

	// Errorf = "проверка не прошла", но продолжаем — соберём все расхождения разом.
	if claims.UserID != id {
		t.Errorf("UserID: получили %v, ждали %v", claims.UserID, id)
	}
	if claims.Role != "admin" {
		t.Errorf("Role: получили %q, ждали %q", claims.Role, "admin")
	}
}

// TestTokenManager_Parse_RejectsInvalid — табличный тест: одна проверка
// ("Parse должен вернуть ErrInvalidToken") на нескольких "плохих" входах.
//
// Смысл всех кейсов один: любой токен, которому нельзя доверять, обязан
// превратиться в единый sentinel ErrInvalidToken и НЕ просочиться дальше.
func TestTokenManager_Parse_RejectsInvalid(t *testing.T) {
	const secret = "test-secret"

	// "Правильный" менеджер — им будем проверять чужие/битые токены.
	valid := NewTokenManager(secret, time.Hour)
	id := uuid.New()

	// 1) Просроченный токен: тот же секрет, но TTL отрицательный —
	//    ExpiresAt оказывается в прошлом ещё в момент выпуска.
	expiredTM := NewTokenManager(secret, -time.Minute)
	expiredToken, err := expiredTM.Issue(id, "user")
	if err != nil {
		t.Fatalf("Issue(expired): %v", err)
	}

	// 2) Токен, подписанный ДРУГИМ секретом — имитация подделки.
	otherTM := NewTokenManager("another-secret", time.Hour)
	otherToken, err := otherTM.Issue(id, "user")
	if err != nil {
		t.Fatalf("Issue(other): %v", err)
	}

	// 3) Токен с алгоритмом "none" — классическая "alg confusion" атака:
	//    атакующий подсовывает токен БЕЗ подписи в надежде, что сервер
	//    его не проверит. Наш keyFunc должен такое отвергнуть.
	noneToken := issueAlgNoneToken(t)

	// Набор кейсов. newName — подтест; token — что скармливаем Parse.
	tests := []struct {
		name  string
		token string
	}{
		{"просроченный", expiredToken},
		{"подпись чужим секретом", otherToken},
		{"битая строка", "not.a.jwt"},
		{"пустая строка", ""},
		{"alg=none (подделка алгоритма)", noneToken},
	}

	for _, tt := range tests {
		// t.Run запускает ПОДТЕСТ: в выводе он виден отдельной строкой,
		// и падение одного кейса не мешает прогнать остальные.
		t.Run(tt.name, func(t *testing.T) {
			_, err := valid.Parse(tt.token)

			// errors.Is (а не ==) — потому что Parse оборачивает ошибку
			// через %w. Проверяем именно "это тот самый sentinel".
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("Parse(%q): получили %v, ждали ErrInvalidToken", tt.name, err)
			}
		})
	}
}

// issueAlgNoneToken — вспомогательная функция: собирает JWT с alg="none"
// (без подписи). Помощники в тестах часто помечают t.Helper() — тогда в
// трейсе падения указывается ВЫЗЫВАЮЩИЙ тест, а не эта функция.
func issueAlgNoneToken(t *testing.T) string {
	t.Helper()

	claims := jwtClaims{
		Role: "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	// UnsafeAllowNoneSignatureType — явное разрешение подписать "без подписи".
	// Библиотека требует его нарочно: чтобы алг=none нельзя было получить случайно.
	s, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("подписать alg=none: %v", err)
	}
	return s
}
