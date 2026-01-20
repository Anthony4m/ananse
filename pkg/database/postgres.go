package database

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresDB wraps a PostgreSQL connection pool
type PostgresDB struct {
	Pool *pgxpool.Pool
}

// PostgresConfig holds database configuration
type PostgresConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	Schema   string
}

// NewPostgresDB creates a new PostgreSQL connection pool
func NewPostgresDB(ctx context.Context, cfg PostgresConfig) (*PostgresDB, error) {
	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable&search_path=%s",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database, cfg.Schema,
	)

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database config: %w", err)
	}

	// Connection pool settings
	poolConfig.MaxConns = 10
	poolConfig.MinConns = 2
	poolConfig.MaxConnLifetime = time.Hour
	poolConfig.MaxConnIdleTime = 30 * time.Minute
	poolConfig.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Verify connection
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	log.Printf("[database] Connected to PostgreSQL at %s:%s/%s (schema: %s)", cfg.Host, cfg.Port, cfg.Database, cfg.Schema)

	return &PostgresDB{Pool: pool}, nil
}

// NewPostgresDBFromEnv creates a PostgreSQL connection from environment variables
func NewPostgresDBFromEnv(ctx context.Context, schema string) (*PostgresDB, error) {
	cfg := PostgresConfig{
		Host:     getEnv("DB_HOST", "postgres"),
		Port:     getEnv("DB_PORT", "5432"),
		User:     getEnv("DB_USER", "ananse"),
		Password: getEnv("DB_PASSWORD", "ananse-secret-pw"),
		Database: getEnv("DB_NAME", "ananse"),
		Schema:   schema,
	}
	return NewPostgresDB(ctx, cfg)
}

// Close closes the database connection pool
func (db *PostgresDB) Close() {
	if db.Pool != nil {
		db.Pool.Close()
		log.Println("[database] PostgreSQL connection closed")
	}
}

// HealthCheck verifies database connectivity
func (db *PostgresDB) HealthCheck(ctx context.Context) error {
	return db.Pool.Ping(ctx)
}

// WaitForDB waits for the database to become available
func WaitForDB(ctx context.Context, cfg PostgresConfig, maxRetries int, retryInterval time.Duration) (*PostgresDB, error) {
	var db *PostgresDB
	var err error

	for i := 0; i < maxRetries; i++ {
		db, err = NewPostgresDB(ctx, cfg)
		if err == nil {
			return db, nil
		}

		log.Printf("[database] Waiting for PostgreSQL... attempt %d/%d: %v", i+1, maxRetries, err)
		time.Sleep(retryInterval)
	}

	return nil, fmt.Errorf("failed to connect to database after %d attempts: %w", maxRetries, err)
}

// WaitForDBFromEnv waits for the database using environment variables
func WaitForDBFromEnv(ctx context.Context, schema string, maxRetries int) (*PostgresDB, error) {
	cfg := PostgresConfig{
		Host:     getEnv("DB_HOST", "postgres"),
		Port:     getEnv("DB_PORT", "5432"),
		User:     getEnv("DB_USER", "ananse"),
		Password: getEnv("DB_PASSWORD", "ananse-secret-pw"),
		Database: getEnv("DB_NAME", "ananse"),
		Schema:   schema,
	}
	return WaitForDB(ctx, cfg, maxRetries, 2*time.Second)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
