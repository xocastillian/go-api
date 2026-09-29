package auth

// refreshRequest — тело запроса для /auth/refresh и /auth/logout.
// Обе ручки принимают ровно одно поле, поэтому общий тип — без дублирования.
// binding:"required" — если поля нет, gin вернёт 400 ещё до вызова handler-а.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// tokenPairResponse — ответ на успешный refresh: НОВАЯ пара токенов.
// Отдельный DTO, а не доменный TokenPair: наружу отдаём только то,
// что решили показать, и в тех именах полей, какие обещали клиенту.
type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// toTokenPairResponse конвертирует доменную пару в DTO ответа.
//
// Поля TokenPair и tokenPairResponse совпадают по именам, типам и порядку,
// поэтому достаточно ПРЕОБРАЗОВАНИЯ ТИПА (tokenPairResponse(p)), а не ручного
// переписывания полей: Go-спека разрешает такую конвертацию, а struct-теги
// (наш json:"...") при сравнении структур игнорируются. Так код короче и
// не «отстаёт», если в одном из типов переименуют поле.
func toTokenPairResponse(p TokenPair) tokenPairResponse {
	return tokenPairResponse(p)
}
