/*
 * Copyright 2026 Maritime Connectivity Platform Consortium
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package persistence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maritimeconnectivity/MMS/mmtp"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

const (
	SessionKindAgent      = "agent"
	SessionKindEdgeRouter = "edge-router"
)

var ErrNotFound = errors.New("persistent state not found")

type Session struct {
	ID             string
	Kind           string
	MRN            string
	DirectMessages bool
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type SQLiteStore struct {
	db *sql.DB
}

func Open(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("database path cannot be empty")
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &SQLiteStore{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = store.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite with %q: %w", statement, err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id                   TEXT PRIMARY KEY,
    kind                 TEXT NOT NULL CHECK (kind IN ('agent', 'edge-router')),
    mrn                  TEXT NOT NULL DEFAULT '',
    reconnect_token_hash BLOB NOT NULL,
    direct_messages      INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    expires_at           INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_kind_mrn ON sessions(kind, mrn);
CREATE INDEX IF NOT EXISTS sessions_expiration ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS subscriptions (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    subject    TEXT NOT NULL,
    PRIMARY KEY (session_id, subject)
);
CREATE INDEX IF NOT EXISTS subscriptions_subject ON subscriptions(subject);

CREATE TABLE IF NOT EXISTS messages (
    uuid       TEXT PRIMARY KEY,
    payload    BLOB NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_expiration ON messages(expires_at);

CREATE TABLE IF NOT EXISTS deliveries (
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_uuid TEXT NOT NULL REFERENCES messages(uuid) ON DELETE CASCADE,
    notified_at  INTEGER,
    PRIMARY KEY (session_id, message_uuid)
);
CREATE INDEX IF NOT EXISTS deliveries_pending_notification
    ON deliveries(session_id, notified_at);

CREATE TABLE IF NOT EXISTS outbox (
    uuid            TEXT PRIMARY KEY,
    payload         BLOB NOT NULL,
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite database: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *SQLiteStore) UpsertSession(ctx context.Context, session Session, reconnectToken string) error {
	if session.ID == "" || reconnectToken == "" {
		return fmt.Errorf("session ID and reconnect token are required")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	if session.ExpiresAt.IsZero() {
		session.ExpiresAt = session.CreatedAt.Add(30 * 24 * time.Hour)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(id, kind, mrn, reconnect_token_hash, direct_messages, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    kind = excluded.kind,
    mrn = excluded.mrn,
    reconnect_token_hash = excluded.reconnect_token_hash,
    direct_messages = excluded.direct_messages,
    expires_at = excluded.expires_at`,
		session.ID, session.Kind, strings.ToLower(session.MRN), tokenHash(reconnectToken),
		session.DirectMessages, session.CreatedAt.Unix(), session.ExpiresAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert session %q: %w", session.ID, err)
	}
	return nil
}

func (s *SQLiteStore) RotateReconnectToken(ctx context.Context, id, reconnectToken string, expiresAt time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET reconnect_token_hash = ?, expires_at = ? WHERE id = ?`,
		tokenHash(reconnectToken), expiresAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("rotate reconnect token: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check reconnect token rotation: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var session Session
	var direct int
	var createdAt, expiresAt int64
	if err := row.Scan(&session.ID, &session.Kind, &session.MRN, &direct, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	session.DirectMessages = direct != 0
	session.CreatedAt = time.Unix(createdAt, 0)
	session.ExpiresAt = time.Unix(expiresAt, 0)
	return &session, nil
}

func (s *SQLiteStore) SessionByToken(ctx context.Context, kind, reconnectToken string) (*Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `
SELECT id, kind, mrn, direct_messages, created_at, expires_at
FROM sessions
WHERE kind = ? AND reconnect_token_hash = ? AND expires_at >= ?`,
		kind, tokenHash(reconnectToken), time.Now().Unix()))
}

func (s *SQLiteStore) SessionByID(ctx context.Context, id string) (*Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `
SELECT id, kind, mrn, direct_messages, created_at, expires_at
FROM sessions WHERE id = ?`, id))
}

func (s *SQLiteStore) Sessions(ctx context.Context, kind string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, kind, mrn, direct_messages, created_at, expires_at
FROM sessions WHERE kind = ? AND expires_at >= ?`, kind, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var result []Session
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan session: %w", scanErr)
		}
		result = append(result, *session)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SetDirectMessages(ctx context.Context, id string, enabled bool) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET direct_messages = ? WHERE id = ?`, enabled, id); err != nil {
		return fmt.Errorf("update direct-message subscription: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Subscribe(ctx context.Context, id, subject string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscriptions(session_id, subject) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		id, subject)
	if err != nil {
		return fmt.Errorf("persist subscription: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Unsubscribe(ctx context.Context, id, subject string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM subscriptions WHERE session_id = ? AND subject = ?`, id, subject); err != nil {
		return fmt.Errorf("delete subscription: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Subscriptions(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT subject FROM subscriptions WHERE session_id = ? ORDER BY subject`, id)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	defer rows.Close()
	var subjects []string
	for rows.Next() {
		var subject string
		if err = rows.Scan(&subject); err != nil {
			return nil, err
		}
		subjects = append(subjects, subject)
	}
	return subjects, rows.Err()
}

func messageExpiry(message *mmtp.MmtpMessage) (int64, error) {
	appMessage := message.GetProtocolMessage().GetSendMessage().GetApplicationMessage()
	if appMessage == nil || appMessage.GetHeader() == nil {
		return 0, fmt.Errorf("message does not contain an application message header")
	}
	return appMessage.GetHeader().GetExpires(), nil
}

func (s *SQLiteStore) QueueMessage(ctx context.Context, sessionIDs []string, message *mmtp.MmtpMessage) error {
	if message == nil || message.GetUuid() == "" {
		return fmt.Errorf("message UUID is required")
	}
	expiresAt, err := messageExpiry(message)
	if err != nil {
		return err
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal queued message: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin queue transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing []byte
	err = tx.QueryRowContext(ctx, `SELECT payload FROM messages WHERE uuid = ?`, message.GetUuid()).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO messages(uuid, payload, expires_at, created_at) VALUES (?, ?, ?, ?)`,
			message.GetUuid(), payload, expiresAt, time.Now().Unix()); err != nil {
			return fmt.Errorf("insert queued message: %w", err)
		}
	case err != nil:
		return fmt.Errorf("check queued message: %w", err)
	default:
		persisted := new(mmtp.MmtpMessage)
		if err = proto.Unmarshal(existing, persisted); err != nil {
			return fmt.Errorf("unmarshal existing message %q: %w", message.GetUuid(), err)
		}
		if !proto.Equal(message, persisted) {
			return fmt.Errorf("message UUID %q already exists with different content", message.GetUuid())
		}
	}

	for _, sessionID := range sessionIDs {
		if _, err = tx.ExecContext(ctx, `
INSERT INTO deliveries(session_id, message_uuid)
VALUES (?, ?)
ON CONFLICT(session_id, message_uuid) DO NOTHING`, sessionID, message.GetUuid()); err != nil {
			return fmt.Errorf("insert delivery for session %q: %w", sessionID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit queued message: %w", err)
	}
	return nil
}

func (s *SQLiteStore) messagesForQuery(ctx context.Context, query string, args ...any) ([]*mmtp.MmtpMessage, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []*mmtp.MmtpMessage
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return nil, err
		}
		message := new(mmtp.MmtpMessage)
		if err = proto.Unmarshal(payload, message); err != nil {
			return nil, fmt.Errorf("unmarshal persisted message: %w", err)
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *SQLiteStore) PendingNotifications(ctx context.Context, id string) ([]*mmtp.MmtpMessage, error) {
	return s.messagesForQuery(ctx, `
SELECT m.payload
FROM deliveries d JOIN messages m ON m.uuid = d.message_uuid
WHERE d.session_id = ? AND d.notified_at IS NULL AND m.expires_at >= ?
ORDER BY m.created_at`, id, time.Now().Unix())
}

func (s *SQLiteStore) MarkNotified(ctx context.Context, id string, uuids []string) error {
	return s.forUUIDs(ctx, uuids, func(tx *sql.Tx, uuid string) error {
		_, err := tx.ExecContext(ctx, `
UPDATE deliveries SET notified_at = ?
WHERE session_id = ? AND message_uuid = ?`, time.Now().Unix(), id, uuid)
		return err
	})
}

func (s *SQLiteStore) FetchMessages(ctx context.Context, id string, uuids []string) ([]*mmtp.MmtpMessage, error) {
	if len(uuids) == 0 {
		return s.messagesForQuery(ctx, `
SELECT m.payload
FROM deliveries d JOIN messages m ON m.uuid = d.message_uuid
WHERE d.session_id = ? AND m.expires_at >= ?
ORDER BY m.created_at`, id, time.Now().Unix())
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(uuids)), ",")
	args := make([]any, 0, len(uuids)+2)
	args = append(args, id, time.Now().Unix())
	for _, uuid := range uuids {
		args = append(args, uuid)
	}
	return s.messagesForQuery(ctx, `
SELECT m.payload
FROM deliveries d JOIN messages m ON m.uuid = d.message_uuid
WHERE d.session_id = ? AND m.expires_at >= ? AND m.uuid IN (`+placeholders+`)
ORDER BY m.created_at`, args...)
}

func (s *SQLiteStore) DeleteDeliveries(ctx context.Context, id string, uuids []string) error {
	return s.forUUIDs(ctx, uuids, func(tx *sql.Tx, uuid string) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM deliveries WHERE session_id = ? AND message_uuid = ?`, id, uuid)
		return err
	})
}

func (s *SQLiteStore) forUUIDs(ctx context.Context, uuids []string, fn func(*sql.Tx, string) error) error {
	if len(uuids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, uuid := range uuids {
		if err = fn(tx, uuid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) PurgeExpired(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE expires_at < ?`, now.Unix()); err != nil {
		return fmt.Errorf("purge messages: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
DELETE FROM messages WHERE NOT EXISTS (
    SELECT 1 FROM deliveries WHERE deliveries.message_uuid = messages.uuid
)`); err != nil {
		return fmt.Errorf("purge delivered messages: %w", err)
	}
	return tx.Commit()
}

// PurgeExpiredSessions removes reconnectable sessions. Call it during startup,
// before live sessions are hydrated, rather than from the periodic message GC.
func (s *SQLiteStore) PurgeExpiredSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.Unix()); err != nil {
		return fmt.Errorf("purge sessions: %w", err)
	}
	return nil
}

func (s *SQLiteStore) PutOutbox(ctx context.Context, message *mmtp.MmtpMessage) error {
	payload, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal outbox message: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO outbox(uuid, payload, next_attempt_at, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(uuid) DO UPDATE SET payload = excluded.payload`,
		message.GetUuid(), payload, time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("insert outbox message: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteOutbox(ctx context.Context, uuid string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM outbox WHERE uuid = ?`, uuid); err != nil {
		return fmt.Errorf("delete outbox message: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Outbox(ctx context.Context) ([]*mmtp.MmtpMessage, error) {
	return s.messagesForQuery(ctx, `
SELECT payload FROM outbox WHERE next_attempt_at <= ? ORDER BY created_at`, time.Now().Unix())
}

func (s *SQLiteStore) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings(key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, []byte(value))
	if err != nil {
		return fmt.Errorf("save setting %q: %w", key, err)
	}
	return nil
}

func (s *SQLiteStore) Setting(ctx context.Context, key string) (string, error) {
	var value []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("load setting %q: %w", key, err)
	}
	return string(value), nil
}

func (s *SQLiteStore) DeleteSetting(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete setting %q: %w", key, err)
	}
	return nil
}
