package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for database/sql
)

// DB wraps *sql.DB so queries can be written with "?" placeholders; they are
// rewritten to PostgreSQL's $1, $2 … (the same approach as sqlx.Rebind).
type DB struct{ *sql.DB }

// Tx is a transaction with the same placeholder rewriting.
type Tx struct{ *sql.Tx }

// rebind turns ? placeholders into $n, leaving quoted text untouched.
func rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n, quote := 0, byte(0)
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (d *DB) Exec(q string, args ...any) (sql.Result, error) { return d.DB.Exec(rebind(q), args...) }
func (d *DB) Query(q string, args ...any) (*sql.Rows, error) { return d.DB.Query(rebind(q), args...) }
func (d *DB) QueryRow(q string, args ...any) *sql.Row        { return d.DB.QueryRow(rebind(q), args...) }
func (d *DB) Begin() (*Tx, error) {
	tx, err := d.DB.Begin()
	return &Tx{tx}, err
}
func (t *Tx) Exec(q string, args ...any) (sql.Result, error) { return t.Tx.Exec(rebind(q), args...) }
func (t *Tx) QueryRow(q string, args ...any) *sql.Row        { return t.Tx.QueryRow(rebind(q), args...) }

func openPostgres(url string) (*DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for { // the database may still be starting (docker compose, rolling deploys)
		err = db.PingContext(ctx)
		if err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db}, nil
}

// migrations are applied in order, once, inside a transaction each. Append new
// ones; never edit an applied migration.
var migrations = []string{
	`CREATE EXTENSION IF NOT EXISTS citext;
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE users (
 id TEXT PRIMARY KEY, username CITEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
 role TEXT NOT NULL DEFAULT 'member', created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE sessions (
 id TEXT PRIMARY KEY, user_id TEXT REFERENCES users(id) ON DELETE CASCADE, is_master BOOLEAN NOT NULL DEFAULT FALSE,
 expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);
CREATE TABLE providers (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, slug CITEXT NOT NULL UNIQUE, type TEXT NOT NULL, base_url TEXT NOT NULL,
 api_key TEXT NOT NULL, headers TEXT NOT NULL DEFAULT '', enabled BOOLEAN NOT NULL DEFAULT TRUE, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE traces (
 id TEXT PRIMARY KEY, request_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '', api_key_name TEXT NOT NULL DEFAULT '',
 api_key_id TEXT NOT NULL DEFAULT '', provider_id TEXT, provider_name TEXT NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
 status_code INTEGER NOT NULL, stream BOOLEAN NOT NULL DEFAULT FALSE, prompt TEXT NOT NULL DEFAULT '', request TEXT NOT NULL DEFAULT '',
 params TEXT NOT NULL DEFAULT '', response TEXT NOT NULL DEFAULT '', finish_reason TEXT NOT NULL DEFAULT '',
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
 cached_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0, latency_ms BIGINT NOT NULL DEFAULT 0,
 ttft_ms DOUBLE PRECISION NOT NULL DEFAULT 0, upstream_latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
 gateway_latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0, cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_traces_created ON traces(created_at DESC);
CREATE INDEX idx_traces_route ON traces(provider_id, model, created_at);
CREATE INDEX idx_traces_key ON traces(api_key_id, created_at);
CREATE INDEX idx_traces_loop ON traces((metadata->>'loop_id'));
CREATE TABLE routing_profiles (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, slug CITEXT NOT NULL UNIQUE, engine TEXT NOT NULL, objective TEXT NOT NULL,
 config_json TEXT NOT NULL, jev_api_key TEXT NOT NULL DEFAULT '', active BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE routing_attempts (
 id BIGSERIAL PRIMARY KEY, provider_id TEXT NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
 latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_routing_attempt_health ON routing_attempts(provider_id, model, created_at DESC);
CREATE TABLE api_keys (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL, last_used_at TIMESTAMPTZ, limits_json TEXT NOT NULL DEFAULT ''
);
CREATE TABLE model_prices (model CITEXT PRIMARY KEY, input DOUBLE PRECISION NOT NULL, output DOUBLE PRECISION NOT NULL);
CREATE TABLE feedback_loops (
 id TEXT PRIMARY KEY, slug CITEXT NOT NULL UNIQUE, config_json TEXT NOT NULL, jev_api_key TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE feedback_votes (
 trace_id TEXT PRIMARY KEY, loop_id TEXT NOT NULL, arm_id TEXT NOT NULL, source TEXT NOT NULL, verdict TEXT NOT NULL,
 up DOUBLE PRECISION NOT NULL, down DOUBLE PRECISION NOT NULL, confidence DOUBLE PRECISION NOT NULL, created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_feedback_votes_arm ON feedback_votes(loop_id, arm_id, created_at DESC);`,
}

// migrate applies pending migrations. An advisory lock makes concurrent
// instances starting together apply them exactly once.
func (s *Store) migrate() error {
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock(7461001)"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock(7461001)") //nolint:errcheck
	if _, err = conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	var current int
	if err = conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&current); err != nil {
		return err
	}
	for v := current + 1; v <= len(migrations); v++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, migrations[v-1]); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", v)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
