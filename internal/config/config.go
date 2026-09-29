package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	AppPort  string
	Postgres PostgresConfig
	JWT      JWTConfig
	CORS     CORSConfig
}

// LoggingConfig — настройки логов (env LOG_LEVEL, LOG_FORMAT).
// Живёт ОТДЕЛЬНО от Config, и это не случайность, а проблема курицы и яйца:
// логгер создаётся в main ДО всего остального — а чтобы сообщить об ошибке
// "конфиг не загрузился", логгер уже должен существовать. Поэтому лог-настройки
// читаются собственной функцией LoadLogging (ниже) раньше полного Load,
// и ошибки конфига логируются уже настроенным логгером.
type LoggingConfig struct {
	Level  string // debug | info | warn | error (дефолт info)
	Format string // text (для глаз) | json (для машин; дефолт)
}

// LoadLogging читает ТОЛЬКО лог-настройки — минимальный ранний конфиг.
// godotenv.Load ошибку игнорирует осознанно (как в Load): без .env работаем
// от переменных окружения, что нормально для docker/CI.
func LoadLogging() LoggingConfig {
	_ = godotenv.Load()
	return LoggingConfig{
		// Валидация значения происходит в parseLevel (main.go): неизвестный
		// уровень → warn в лог + info как безопасный дефолт.
		Level:  getEnv("LOG_LEVEL", "info"),
		Format: getEnv("LOG_FORMAT", "json"),
	}
}

// CORSConfig — настройки кросс-доменных запросов (для браузерного фронтенда).
// CORS защищает не сервер, а браузер: он разрешает JS одной страницы
// обращаться к API на ДРУГОМ origin'е. Список origin'ов держим явным
// белым списком — никаких "*", иначе любой сайт получит доступ к API.
type CORSConfig struct {
	// AllowedOrigins — разрешённые origin'ы (схема+хост+порт), напр.
	// ["http://localhost:3000", "https://app.example.com"].
	// Пустой список означает "CORS выключен" — безопасный дефолт.
	AllowedOrigins []string
}

// JWTConfig — настройки токенов сессии.
type JWTConfig struct {
	Secret     string        // ключ подписи (из env, обязателен)
	TTL        time.Duration // срок жизни ACCESS-токена (короткий)
	RefreshTTL time.Duration // срок жизни REFRESH-токена (длинный)

	// CleanupInterval — период фоновой чистки протухших refresh-токенов
	// (запускается в main.go через authService.RunCleanup). Период —
	// компромисс: чаще = чище таблица, дороже БД. Час — разумный дефолт.
	CleanupInterval time.Duration
}

type PostgresConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DB       string

	// SSLMode — режим TLS-соединения с БД (env POSTGRES_SSLMODE).
	//   disable      — без шифрования. Дефолт для ЛОКАЛЬНОЙ разработки:
	//                  трафик не покидает машину, шифровать нечего.
	//   require      — шифрование обязательно, сертификат не проверяется.
	//   verify-full  — шифрование + проверка сертификата сервера. ТОЛЬКО так
	//                  в проде, где БД за сетью: без него трафик (включая
	//                  пароль в DSN!) идёт открытым текстом.
	// Раньше "disable" был захардкожен — и «переехал в прод» бы незаметно.
	SSLMode string

	// MaxConns — верхний предел одновременных соединений пула к БД
	// (env PGX_MAX_CONNS). Всё, что не влезло, стоит в очереди пула —
	// при узкой очереди растёт p99 даже на дешёвых запросах.
	// Подбирается замером, не "чем больше тем лучше": каждое соединение —
	// процесс на стороне Postgres (память + переключения контекста),
	// и сервер БД у нас всё равно один на все окружения.
	MaxConns int32

	// MinConns — сколько соединений держать открытыми ПРОСТО так
	// (env PGX_MIN_CONNS), чтобы первый запрос не платил за открытие
	// коннекта (TCP-хендшейк + auth ≈ единицы миллисекунд).
	MinConns int32
}

// DSN собирает строку подключения из полей. Метод на структуре —
// так DSN живёт рядом с данными, из которых он собирается.
func (p PostgresConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=%s",
		p.User, p.Password, p.Host, p.Port, p.DB, p.SSLMode,
	)
}

func Load() (*Config, error) {
	// В докере .env не нужен — переменные придут от окружения.
	// Локально — подгружаем файл. Ошибку игнорируем осознанно.
	_ = godotenv.Load()

	cfg := &Config{
		AppPort: getEnv("APP_PORT", "8080"),
		Postgres: PostgresConfig{
			Host:     getEnv("POSTGRES_HOST", "localhost"),
			Port:     getEnv("POSTGRES_PORT", "5432"),
			User:     getEnv("POSTGRES_USER", ""),
			Password: getEnv("POSTGRES_PASSWORD", ""),
			DB:       getEnv("POSTGRES_DB", ""),
			// Дефолт disable — для локальной разработки. В проде задаётся
			// явно: POSTGRES_SSLMODE=verify-full.
			SSLMode: getEnv("POSTGRES_SSLMODE", "disable"),
			// Размеры пула — прежние дефолты, что были захардкожены в
			// NewPool; поведение "из коробки" не изменилось, но теперь
			// настраивается без пересборки.
			MaxConns: getEnvInt32("PGX_MAX_CONNS", 10),
			MinConns: getEnvInt32("PGX_MIN_CONNS", 2),
		},
		JWT: JWTConfig{
			Secret: getEnv("JWT_SECRET", ""),
			// Access живёт мало: он летает в каждом запросе, поэтому
			// утечка должна быть "дешёвой" — урон на минуты.
			TTL: getDuration("JWT_TTL", 15*time.Minute),
			// Refresh живёт долго и хранится в БД — его можно отозвать.
			// Он и обеспечивает "не выкидывать пользователя".
			RefreshTTL: getDuration("REFRESH_TTL", 30*24*time.Hour),
			// Период фоновой чистки протухших refresh-токенов.
			CleanupInterval: getDuration("REFRESH_CLEANUP_INTERVAL", time.Hour),
		},
		CORS: CORSConfig{
			// Дефолт — типичный dev-фронтенд на :3000, чтобы "из коробки"
			// работала связка. В проде задаётся явным списком через env.
			AllowedOrigins: getList("CORS_ALLOWED_ORIGINS", "http://localhost:3000"),
		},
	}

	// fail fast: без секрета токены не подписать — стартовать бессмысленно.
	// Лучше упасть на старте с понятным сообщением, чем отдавать битые токены.
	if cfg.JWT.Secret == "" {
		return nil, errors.New("JWT_SECRET is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getDuration читает длительность из env в формате Go (например "24h", "15m").
// Если переменная не задана или не парсится — берём fallback.
func getDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// getEnvInt32 читает целое число из env. Не задано или не парсится → fallback.
// int32 — потому что pgxpool.MaxConns/MinConns именно этого типа.
func getEnvInt32(key string, fallback int32) int32 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return fallback
	}
	return int32(n)
}

// getList читает из env список через запятую. Обрезает пробелы и выкидывает
// пустые элементы. Отсутствующая/пустая переменная → fallback (тоже список).
// Удобно для CORS_ALLOWED_ORIGINS=http://a.com,http://b.com.
func getList(key, fallback string) []string {
	v := getEnv(key, fallback)
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
