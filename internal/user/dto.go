package user

// registerRequest — что присылает клиент на регистрацию.
// Теги json: как называются поля в JSON.
// Теги binding: правила валидации gin (required, email, min=...).
//
// max=72 на пароле — НЕ произвольная цифра: bcrypt молча ОБРЕЗАЕТ пароль
// на 72-м байте, и пароль из 100 символов фактически защищён первыми 72.
// Ограничив на входе, мы делаем лимит честным (клиент знает о нём заранее),
// а не сюрпризом внутри криптографии.
// Нюанс: validator считает РУНЫ, bcrypt — БАЙТЫ; для кириллицы (2 байта
// на руну) лимит 72 руны пропустит до 144 байт. Для учебного проекта
// допущение принято; строгий вариант — проверка len() в сервисе.
type registerRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8,max=72"`
}

// loginRequest — что присылает клиент на логин.
type loginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// changePasswordRequest — тело запроса PATCH /me/password.
//
//   - old_password — текущий пароль. Нужен как подтверждение права на смену:
//     наличие одного лишь access-токена не даёт менять пароль молча.
//   - new_password — новый пароль. min=8/max=72 — то же правило, что при
//     регистрации: политика длины пароля задаётся в ОДНОМ стиле по всему
//     приложению (про 72 — см. комментарий у registerRequest).
//
// Отдельный DTO, а не переиспользование loginRequest: у них разные контракты
// (тут нет email, зато есть старое/новое имя полей), и связаны они быть не должны —
// изменение одного не должно тянуть за собой другое.
type changePasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=72"`
}

// listUsersQuery — query-параметры админского списка (?limit=&offset=).
// form:"..." указывает gin, откуда читать значение (из query string).
// binding:"omitempty,..." — параметр НЕобязателен: пустое значение не ошибка,
// дефолт подставит handler. min/max защищают от отрицательных и гигантских величин.
type listUsersQuery struct {
	Limit  int `form:"limit" binding:"omitempty,min=1,max=100"`
	Offset int `form:"offset" binding:"omitempty,min=0"`
}

// usersResponse — страница пользователей + метаданные пагинации,
// чтобы клиент понимал, какой именно срез он получил.
type usersResponse struct {
	Users  []userResponse `json:"users"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// userResponse — что мы отдаём клиенту. Здесь НЕТ пароля/хеша вообще.
type userResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// authResponse — ответ на успешный логин/регистрацию.
// Отдаём ПАРУ токенов + профиль:
//   - access_token  — короткий, для запросов (в Authorization: Bearer);
//   - refresh_token — длинный, только для /auth/refresh.
//
// Профиль рядом, чтобы клиенту не понадобился лишний запрос к /me.
type authResponse struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	User         userResponse `json:"user"`
}

// toResponse конвертирует доменную модель в DTO ответа.
func toResponse(u User) userResponse {
	return userResponse{
		ID:    u.ID.String(),
		Email: u.Email,
		Role:  string(u.Role),
	}
}

// toResponses конвертирует срез доменных моделей в срез DTO.
// make(..., 0, len) гарантирует, что при пустом входе вернётся пустой срез,
// а не nil: в JSON это [] вместо null — клиенту проще (не нужно проверять null).
func toResponses(users []User) []userResponse {
	out := make([]userResponse, 0, len(users))
	for _, u := range users {
		out = append(out, toResponse(u))
	}
	return out
}
