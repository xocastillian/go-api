package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/timqa/my-rest-api/internal/apperr"
	"github.com/timqa/my-rest-api/internal/platform/postgres"
)

// Repository — слой доступа к данным таблицы users.
// Работает через postgres.DBTX — абстракцию над "тем, кто умеет ходить в БД":
// это может быть ПУЛ соединений (обычная работа сервера) или ТРАНЗАКЦИЯ
// (интеграционные тесты с откатом). Никакой ORM: мы сами пишем SQL и сами
// читаем результат. Имя без префикса User — пакет user уже даёт контекст.
type Repository struct {
	db postgres.DBTX
}

// NewRepository — конструктор. DBTX передаём снаружи (dependency injection),
// а не создаём внутри — так репозиторий легко подменить в тестах
// (например, подсунуть транзакцию вместо пула).
func NewRepository(db postgres.DBTX) *Repository {
	return &Repository{db: db}
}

// Create вставляет нового пользователя и возвращает его с полями,
// сгенерированными БД (id, created_at, updated_at).
func (r *Repository) Create(ctx context.Context, u User) (User, error) {
	// $1, $2, ... — плейсхолдеры. Значения передаются ОТДЕЛЬНО от SQL,
	// поэтому SQL-инъекция невозможна (в отличие от склейки строк).
	// RETURNING отдаёт строку обратно после INSERT — не нужен второй SELECT.
	const query = `
		INSERT INTO users (email, password_hash, role)
		VALUES ($1, $2, $3)
		RETURNING id, email, password_hash, role, created_at, updated_at
	`

	var created User
	// QueryRow — потому что запрос возвращает ОДНУ строку (RETURNING).
	// Scan читает колонки в поля структуры ПО ПОРЯДКУ.
	err := r.db.QueryRow(ctx, query, u.Email, u.PasswordHash, u.Role).
		Scan(
			&created.ID,
			&created.Email,
			&created.PasswordHash,
			&created.Role,
			&created.CreatedAt,
			&created.UpdatedAt,
		)
	if err != nil {
		// Нарушение UNIQUE по email в Postgres имеет код 23505.
		// Переводим "сырую" ошибку БД в доменную — чтобы service/handler
		// не знали про коды Postgres.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return User{}, fmt.Errorf("create user: %w", apperr.ErrEmailTaken)
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return created, nil
}

// GetByEmail ищет пользователя по email (нужно для регистрации и логина).
func (r *Repository) GetByEmail(ctx context.Context, email string) (User, error) {
	const query = `
		SELECT id, email, password_hash, role, created_at, updated_at
		FROM users
		WHERE email = $1
	`

	var u User
	err := r.db.QueryRow(ctx, query, email).
		Scan(
			&u.ID,
			&u.Email,
			&u.PasswordHash,
			&u.Role,
			&u.CreatedAt,
			&u.UpdatedAt,
		)
	if err != nil {
		// pgx.ErrNoRows — сигнал "строк нет". Переводим в доменную apperr.ErrNotFound.
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, fmt.Errorf("get user by email: %w", apperr.ErrNotFound)
		}
		return User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

// GetByID ищет пользователя по id (понадобится при загрузке юзера из JWT).
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (User, error) {
	const query = `
		SELECT id, email, password_hash, role, created_at, updated_at
		FROM users
		WHERE id = $1
	`

	var u User
	err := r.db.QueryRow(ctx, query, id).
		Scan(
			&u.ID,
			&u.Email,
			&u.PasswordHash,
			&u.Role,
			&u.CreatedAt,
			&u.UpdatedAt,
		)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, fmt.Errorf("get user by id: %w", apperr.ErrNotFound)
		}
		return User{}, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}

// UpdatePassword меняет хеш пароля пользователя.
// На вход приходит УЖЕ готовый хеш — его посчитал сервис. Репозиторий про
// bcrypt не знает вообще: для него это просто текстовая колонка. Такое
// разделение важно — криптография это доменное/сервисное правило, а не
// забота слоя доступа к данным.
func (r *Repository) UpdatePassword(ctx context.Context, id uuid.UUID, passwordHash string) error {
	// updated_at НЕ трогаем намеренно: его автоматически проставит
	// триггер users_set_updated_at (миграция 00001_init) на любом UPDATE.
	// Правило живёт в схеме, поэтому его нельзя забыть здесь или в любом
	// другом будущем UPDATE — и не нужно дублировать в каждом запросе.
	const query = `
		UPDATE users
		SET password_hash = $1
		WHERE id = $2
	`

	// Exec, а НЕ QueryRow: строка обратно не нужна, нужен лишь факт записи.
	// Exec возвращает CommandTag, из которого можно узнать число затронутых строк.
	tag, err := r.db.Exec(ctx, query, passwordHash, id)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	// RowsAffected == 0 означает "ни одна строка не подошла под WHERE".
	// Трактовать это надо как "пользователя с таким id нет" — иначе молча
	// "ничего не сделаем" и вернём успех, хотя пароль не изменён.
	// Такой же подход ловит удалённого юзера: смену пароля ему уже не сделать.
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update password: %w", apperr.ErrNotFound)
	}
	return nil
}

// Delete физически удаляет пользователя по id.
// Каскад делаем НЕ здесь, а на уровне схемы: у внешних ключей notes.user_id
// и refresh_tokens.user_id стоит ON DELETE CASCADE. Поэтому одной этой командой
// Postgres снесёт и заметки, и все сессии пользователя — атомарно, в рамках
// одной транзакции. Коду не нужно вручную чистить дочерние таблицы, и он не
// может "забыть" это сделать: целостность гарантирует сама БД.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	const query = `DELETE FROM users WHERE id = $1`

	// Exec: строка обратно не нужна, нужен лишь факт удаления.
	tag, err := r.db.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}

	// RowsAffected == 0 → такого id не было. Возвращаем ErrNotFound,
	// чтобы клиент получил 404, а не ложный "успех" на несуществующем юзере.
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete user: %w", apperr.ErrNotFound)
	}
	return nil
}

// List возвращает страницу пользователей: limit штук, пропустив offset.
// Из-за потенциально БОЛЬШОГО числа строк используем Query (не QueryRow):
// он отдаёт итератор rows, по которому идём по одной записи.
//
// SQL остаётся здесь, внутри репозитория: вызывающие слои (service/handler)
// не должны знать ни про LIMIT/OFFSET, ни про сортировку.
func (r *Repository) List(ctx context.Context, limit, offset int) ([]User, error) {
	// ORDER BY обязателен для стабильной пагинации: без явного порядка
	// строки могут прийти в разном порядке между запросами, и страницы
	// начнут "перемешиваться". Сортируем по created_at DESC — новые сверху.
	const query = `
		SELECT id, email, password_hash, role, created_at, updated_at
		FROM users
		ORDER BY created_at DESC, id DESC
		LIMIT $1 OFFSET $2
	`

	// Query (в отличие от QueryRow) возвращает итератор rows,
	// по которому мы пройдём по строкам результата.
	rows, err := r.db.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	// ВАЖНО: rows.Close() освобождает соединение обратно в пул.
	// Без него коннекшн утечёт — пул исчерпается и весь сервис встанет.
	defer rows.Close()

	// Заранее выделяем ровно под limit — избегаем реаллокаций при append.
	users := make([]User, 0, limit)

	// rows.Next() возвращает true, пока есть ещё строки.
	for rows.Next() {
		var u User
		// Scan здесь — метод rows, а не pool: читаем текущую строку итератора.
		if err := rows.Scan(
			&u.ID,
			&u.Email,
			&u.PasswordHash,
			&u.Role,
			&u.CreatedAt,
			&u.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}

	// rows.Err() проверяет ошибки, случившиеся ВО ВРЕМЯ итерации
	// (обрыв соединения, таймаут и т.п.). rows.Next() их "проглатывает",
	// возвращая false, поэтому без этой проверки сбой был бы незаметен.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}

	return users, nil
}
