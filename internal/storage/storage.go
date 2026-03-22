package storage

import (
	"crypto/rand"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func New(databasePath string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", filepath.ToSlash(databasePath))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Initialize() error {
	schema := `
CREATE TABLE IF NOT EXISTS logical_models (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'active',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS backends (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	logical_model_id INTEGER NOT NULL REFERENCES logical_models(id) ON DELETE CASCADE,
	name TEXT NOT NULL UNIQUE,
	base_url TEXT NOT NULL,
	upstream_model_name TEXT NOT NULL,
	api_key TEXT NOT NULL DEFAULT '',
	healthcheck_path TEXT NOT NULL DEFAULT '/health',
	priority INTEGER NOT NULL DEFAULT 100,
	weight INTEGER NOT NULL DEFAULT 100,
	timeout_ms INTEGER NOT NULL DEFAULT 120000,
	status TEXT NOT NULL DEFAULT 'active',
	health_status TEXT NOT NULL DEFAULT 'unknown',
	last_error TEXT NOT NULL DEFAULT '',
	last_checked_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	key_hash TEXT NOT NULL UNIQUE,
	allowed_models TEXT NOT NULL DEFAULT '[]',
	enabled INTEGER NOT NULL DEFAULT 1,
	last_used_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS request_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL,
	api_key_name TEXT NOT NULL DEFAULT '',
	logical_model_name TEXT NOT NULL,
	backend_name TEXT NOT NULL DEFAULT '',
	backend_url TEXT NOT NULL DEFAULT '',
	streamed INTEGER NOT NULL DEFAULT 0,
	status_code INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER,
	completion_tokens INTEGER,
	total_tokens INTEGER,
	success INTEGER NOT NULL DEFAULT 0,
	error_message TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) ListLogicalModels(ctx context.Context) ([]LogicalModel, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, description, status, created_at, updated_at
FROM logical_models
ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []LogicalModel
	for rows.Next() {
		var item LogicalModel
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetLogicalModelByName(ctx context.Context, name string) (*LogicalModel, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, description, status, created_at, updated_at
FROM logical_models
WHERE name = ?`, strings.TrimSpace(name))
	var item LogicalModel
	if err := row.Scan(&item.ID, &item.Name, &item.Description, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Store) GetLogicalModelByID(ctx context.Context, id int64) (*LogicalModel, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, description, status, created_at, updated_at
FROM logical_models
WHERE id = ?`, id)
	var item LogicalModel
	if err := row.Scan(&item.ID, &item.Name, &item.Description, &item.Status, &item.CreatedAt, &item.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Store) CreateLogicalModel(ctx context.Context, name, description, status string) error {
	now := nowStamp()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO logical_models(name, description, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`,
		strings.TrimSpace(name),
		strings.TrimSpace(description),
		normalizeStatus(status, "active"),
		now,
		now,
	)
	return err
}

func (s *Store) UpdateLogicalModel(ctx context.Context, id int64, name, description, status string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE logical_models
SET name = ?, description = ?, status = ?, updated_at = ?
WHERE id = ?`,
		strings.TrimSpace(name),
		strings.TrimSpace(description),
		normalizeStatus(status, "active"),
		nowStamp(),
		id,
	)
	return err
}

func (s *Store) DeleteLogicalModel(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM logical_models WHERE id = ?`, id)
	return err
}

func (s *Store) UpsertLogicalModel(ctx context.Context, name, description, status string) (int64, error) {
	existing, err := s.GetLogicalModelByName(ctx, name)
	if err != nil {
		return 0, err
	}
	if existing == nil {
		if err := s.CreateLogicalModel(ctx, name, description, status); err != nil {
			return 0, err
		}
		existing, err = s.GetLogicalModelByName(ctx, name)
		if err != nil {
			return 0, err
		}
	}
	if existing == nil {
		return 0, errors.New("model upsert failed")
	}
	if err := s.UpdateLogicalModel(ctx, existing.ID, name, description, status); err != nil {
		return 0, err
	}
	return existing.ID, nil
}

func (s *Store) ListBackends(ctx context.Context) ([]Backend, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	b.id, b.logical_model_id, m.name AS logical_model_name, b.name, b.base_url, b.upstream_model_name,
	b.api_key, b.healthcheck_path, b.priority, b.weight, b.timeout_ms, b.status,
	b.health_status, b.last_error, b.last_checked_at, b.created_at, b.updated_at
FROM backends b
JOIN logical_models m ON m.id = b.logical_model_id
ORDER BY m.name ASC, b.priority ASC, b.name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Backend
	for rows.Next() {
		var item Backend
		if err := scanBackend(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListBackendsForModel(ctx context.Context, logicalModelName string) ([]Backend, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	b.id, b.logical_model_id, m.name AS logical_model_name, b.name, b.base_url, b.upstream_model_name,
	b.api_key, b.healthcheck_path, b.priority, b.weight, b.timeout_ms, b.status,
	b.health_status, b.last_error, b.last_checked_at, b.created_at, b.updated_at
FROM backends b
JOIN logical_models m ON m.id = b.logical_model_id
WHERE m.name = ?
ORDER BY b.priority ASC, b.name ASC`, logicalModelName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Backend
	for rows.Next() {
		var item Backend
		if err := scanBackend(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetBackendByID(ctx context.Context, id int64) (*Backend, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
	b.id, b.logical_model_id, m.name AS logical_model_name, b.name, b.base_url, b.upstream_model_name,
	b.api_key, b.healthcheck_path, b.priority, b.weight, b.timeout_ms, b.status,
	b.health_status, b.last_error, b.last_checked_at, b.created_at, b.updated_at
FROM backends b
JOIN logical_models m ON m.id = b.logical_model_id
WHERE b.id = ?`, id)
	var item Backend
	if err := scanBackend(row, &item); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Store) GetBackendByName(ctx context.Context, name string) (*Backend, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
	b.id, b.logical_model_id, m.name AS logical_model_name, b.name, b.base_url, b.upstream_model_name,
	b.api_key, b.healthcheck_path, b.priority, b.weight, b.timeout_ms, b.status,
	b.health_status, b.last_error, b.last_checked_at, b.created_at, b.updated_at
FROM backends b
JOIN logical_models m ON m.id = b.logical_model_id
WHERE b.name = ?`, strings.TrimSpace(name))
	var item Backend
	if err := scanBackend(row, &item); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (s *Store) CreateBackend(ctx context.Context, backend Backend) error {
	now := nowStamp()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO backends(
	logical_model_id, name, base_url, upstream_model_name, api_key, healthcheck_path,
	priority, weight, timeout_ms, status, health_status, last_error, last_checked_at,
	created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'unknown', '', '', ?, ?)`,
		backend.LogicalModelID,
		strings.TrimSpace(backend.Name),
		normalizeURL(backend.BaseURL),
		strings.TrimSpace(backend.UpstreamModelName),
		strings.TrimSpace(backend.APIKey),
		defaultString(strings.TrimSpace(backend.HealthcheckPath), "/health"),
		backend.Priority,
		maxInt(backend.Weight, 1),
		maxInt(backend.TimeoutMs, 1000),
		normalizeBackendStatus(backend.Status),
		now,
		now,
	)
	return err
}

func (s *Store) UpdateBackend(ctx context.Context, backend Backend) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE backends
SET logical_model_id = ?, name = ?, base_url = ?, upstream_model_name = ?, api_key = ?,
	healthcheck_path = ?, priority = ?, weight = ?, timeout_ms = ?, status = ?, updated_at = ?
WHERE id = ?`,
		backend.LogicalModelID,
		strings.TrimSpace(backend.Name),
		normalizeURL(backend.BaseURL),
		strings.TrimSpace(backend.UpstreamModelName),
		strings.TrimSpace(backend.APIKey),
		defaultString(strings.TrimSpace(backend.HealthcheckPath), "/health"),
		backend.Priority,
		maxInt(backend.Weight, 1),
		maxInt(backend.TimeoutMs, 1000),
		normalizeBackendStatus(backend.Status),
		nowStamp(),
		backend.ID,
	)
	return err
}

func (s *Store) UpsertBackend(ctx context.Context, backend Backend) error {
	existing, err := s.GetBackendByName(ctx, backend.Name)
	if err != nil {
		return err
	}
	if existing == nil {
		return s.CreateBackend(ctx, backend)
	}
	backend.ID = existing.ID
	return s.UpdateBackend(ctx, backend)
}

func (s *Store) DeleteBackend(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backends WHERE id = ?`, id)
	return err
}

func (s *Store) SetBackendStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE backends
SET status = ?, updated_at = ?
WHERE id = ?`,
		normalizeBackendStatus(status),
		nowStamp(),
		id,
	)
	return err
}

func (s *Store) UpdateBackendHealth(ctx context.Context, id int64, healthStatus, lastError string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE backends
SET health_status = ?, last_error = ?, last_checked_at = ?, updated_at = ?
WHERE id = ?`,
		normalizeHealthStatus(healthStatus),
		trimText(lastError, 500),
		nowStamp(),
		nowStamp(),
		id,
	)
	return err
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, key_hash, allowed_models, enabled, last_used_at, created_at
FROM api_keys
ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []APIKeyRecord
	for rows.Next() {
		var item APIKeyRecord
		var enabled int
		if err := rows.Scan(&item.ID, &item.Name, &item.KeyHash, &item.AllowedModels, &enabled, &item.LastUsedAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled == 1
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) CreateAPIKey(ctx context.Context, name string, allowedModels []string, enabled bool) (string, error) {
	rawKey := "sk-agw-" + randomToken(32)
	allowedBytes, err := json.Marshal(normalizeAllowedModels(allowedModels))
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO api_keys(name, key_hash, allowed_models, enabled, last_used_at, created_at)
VALUES (?, ?, ?, ?, '', ?)`,
		strings.TrimSpace(name),
		hashToken(rawKey),
		string(allowedBytes),
		boolToInt(enabled),
		nowStamp(),
	)
	if err != nil {
		return "", err
	}
	return rawKey, nil
}

func (s *Store) AuthenticateAPIKey(ctx context.Context, rawKey string) (*APIKeyAuth, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, allowed_models
FROM api_keys
WHERE key_hash = ? AND enabled = 1`, hashToken(strings.TrimSpace(rawKey)))
	var auth APIKeyAuth
	var allowedJSON string
	if err := row.Scan(&auth.ID, &auth.Name, &allowedJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(allowedJSON), &auth.AllowedModels); err != nil {
		return nil, err
	}
	return &auth, nil
}

func (s *Store) TouchAPIKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, nowStamp(), id)
	return err
}

func (s *Store) SetAPIKeyEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET enabled = ? WHERE id = ?`, boolToInt(enabled), id)
	return err
}

func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return err
}

func (s *Store) LogRequest(ctx context.Context, logItem RequestLog) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO request_logs(
	request_id, api_key_name, logical_model_name, backend_name, backend_url, streamed,
	status_code, latency_ms, prompt_tokens, completion_tokens, total_tokens, success,
	error_message, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		logItem.RequestID,
		logItem.APIKeyName,
		logItem.LogicalModelName,
		logItem.BackendName,
		logItem.BackendURL,
		boolToInt(logItem.Streamed),
		logItem.StatusCode,
		logItem.LatencyMs,
		logItem.PromptTokens,
		logItem.CompletionTokens,
		logItem.TotalTokens,
		boolToInt(logItem.Success),
		trimText(logItem.ErrorMessage, 500),
		nowStamp(),
	)
	return err
}

func (s *Store) RecentLogs(ctx context.Context, limit int) ([]RequestLog, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, request_id, api_key_name, logical_model_name, backend_name, backend_url, streamed,
	status_code, latency_ms, prompt_tokens, completion_tokens, total_tokens, success,
	error_message, created_at
FROM request_logs
ORDER BY id DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []RequestLog
	for rows.Next() {
		var item RequestLog
		var streamed, success int
		if err := rows.Scan(
			&item.ID,
			&item.RequestID,
			&item.APIKeyName,
			&item.LogicalModelName,
			&item.BackendName,
			&item.BackendURL,
			&streamed,
			&item.StatusCode,
			&item.LatencyMs,
			&item.PromptTokens,
			&item.CompletionTokens,
			&item.TotalTokens,
			&success,
			&item.ErrorMessage,
			&item.CreatedAt,
		); err != nil {
			return nil, err
		}
		item.Streamed = streamed == 1
		item.Success = success == 1
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logical_models`).Scan(&stats.ModelsCount); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backends`).Scan(&stats.BackendsCount); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backends WHERE health_status = 'healthy'`).Scan(&stats.HealthyBackends); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`).Scan(&stats.RequestCount); err != nil {
		return stats, err
	}
	return stats, nil
}

type ExportDocument struct {
	LogicalModels []LogicalModel `json:"logical_models"`
	Backends      []Backend      `json:"backends"`
}

func (s *Store) ExportConfiguration(ctx context.Context) (ExportDocument, error) {
	models, err := s.ListLogicalModels(ctx)
	if err != nil {
		return ExportDocument{}, err
	}
	backends, err := s.ListBackends(ctx)
	if err != nil {
		return ExportDocument{}, err
	}
	return ExportDocument{
		LogicalModels: models,
		Backends:      backends,
	}, nil
}

func scanBackend(scanner interface {
	Scan(dest ...any) error
}, item *Backend) error {
	return scanner.Scan(
		&item.ID,
		&item.LogicalModelID,
		&item.LogicalModelName,
		&item.Name,
		&item.BaseURL,
		&item.UpstreamModelName,
		&item.APIKey,
		&item.HealthcheckPath,
		&item.Priority,
		&item.Weight,
		&item.TimeoutMs,
		&item.Status,
		&item.HealthStatus,
		&item.LastError,
		&item.LastCheckedAt,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
}

func nowStamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func randomToken(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buffer := make([]byte, length)
	_, _ = rand.Read(buffer)
	for i := range buffer {
		buffer[i] = alphabet[int(buffer[i])%len(alphabet)]
	}
	return string(buffer)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeAllowedModels(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	slices.Sort(out)
	return out
}

func normalizeURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func normalizeStatus(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "active", "inactive":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return fallback
	}
}

func normalizeBackendStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "active", "draining", "disabled":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "active"
	}
}

func normalizeHealthStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "healthy", "unhealthy", "unknown":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func trimText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func maxInt(value, floor int) int {
	if value < floor {
		return floor
	}
	return value
}
