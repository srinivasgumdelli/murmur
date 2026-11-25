// Package sqlitestore stores Murmur memory in a SQLite database.
package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/memory"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

var _ memory.Store = (*Store)(nil)

// OpenDB opens a file-backed SQLite pool with connection settings suitable for
// both memory and chat storage.
func OpenDB(ctx context.Context, path string) (*sql.DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(absPath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	params := url.Values{
		"_pragma": {"busy_timeout(5000)", "foreign_keys(1)"},
		"_txlock": {"immediate"},
	}
	uri := url.URL{Scheme: "file", Path: uriPath, RawQuery: params.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}

	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if journalMode != "wal" {
		_ = db.Close()
		return nil, fmt.Errorf("enable SQLite WAL: got journal mode %q", journalMode)
	}
	return db, nil
}

// Open creates or opens a SQLite memory database and applies its schema.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := OpenDB(ctx, path)
	if err != nil {
		return nil, err
	}
	store, err := New(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// New applies the memory schema to a pool opened with OpenDB.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("apply SQLite memory schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

const columns = `id, scope, COALESCE(key, ''), kind, body, tags, author,
    COALESCE(session_id, ''), provenance, COALESCE(promoted_by, ''),
    supersedes, created_at, expires_at`

type scanner interface {
	Scan(...any) error
}

func scanMemory(row scanner, trailing ...any) (memory.Memory, error) {
	var m memory.Memory
	var tags string
	var micros int64
	var expires sql.NullInt64
	dest := []any{
		&m.ID, &m.Scope, &m.Key, &m.Kind, &m.Body, &tags, &m.Author,
		&m.SessionID, &m.Provenance, &m.PromotedBy, &m.Supersedes, &micros, &expires,
	}
	if err := row.Scan(append(dest, trailing...)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return memory.Memory{}, memory.ErrNotFound
		}
		return memory.Memory{}, err
	}
	m.CreatedAt = time.UnixMicro(micros).UTC()
	if expires.Valid {
		at := time.UnixMicro(expires.Int64).UTC()
		expiry := at.UTC().Truncate(time.Microsecond)
		m.ExpiresAt = &expiry
	}
	if err := json.Unmarshal([]byte(tags), &m.Tags); err != nil {
		return memory.Memory{}, err
	}
	return m, nil
}

func (s *Store) Put(ctx context.Context, m memory.Memory, scopeCap int) (memory.Memory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Memory{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := put(ctx, tx, m, scopeCap)
	if err != nil {
		return memory.Memory{}, err
	}
	return stored, tx.Commit()
}

func put(ctx context.Context, tx *sql.Tx, m memory.Memory, scopeCap int) (memory.Memory, error) {
	m.Supersedes = nil
	at := m.CreatedAt.UnixMicro()
	var replacesCurrent bool
	if m.Key != "" {
		var previousID int
		err := tx.QueryRowContext(ctx, `
			SELECT id, `+current+` FROM memories
			WHERE scope = ? AND key = ? ORDER BY id DESC LIMIT 1`,
			at, m.Scope, m.Key).Scan(&previousID, &replacesCurrent)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return memory.Memory{}, err
		}
		if err == nil {
			m.Supersedes = &previousID
		}
	}
	if scopeCap > 0 && m.Kind != memory.KindNone && !replacesCurrent {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM memories WHERE scope = ? AND kind != 'none' AND `+current,
			m.Scope, at).Scan(&count); err != nil {
			return memory.Memory{}, err
		}
		if count >= scopeCap {
			return memory.Memory{}, memory.ErrScopeFull
		}
	}
	if m.Supersedes != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE memories SET superseded_at = COALESCE(superseded_at, ?) WHERE id = ?`,
			at, *m.Supersedes); err != nil {
			return memory.Memory{}, err
		}
	}

	if m.Tags == nil {
		m.Tags = []string{}
	}
	tags, err := json.Marshal(m.Tags)
	if err != nil {
		return memory.Memory{}, err
	}
	var expires any
	if m.ExpiresAt != nil {
		expires = m.ExpiresAt.UnixMicro()
	}
	return scanMemory(tx.QueryRowContext(ctx, `
		INSERT INTO memories(scope, key, kind, body, tags, author, session_id,
		                     provenance, promoted_by, supersedes, created_at, expires_at)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?, ?)
		RETURNING `+columns,
		m.Scope, m.Key, m.Kind, m.Body, string(tags), m.Author, m.SessionID,
		m.Provenance, m.PromotedBy, m.Supersedes, at, expires))
}

func (s *Store) Get(ctx context.Context, id int, history bool) ([]memory.Memory, error) {
	if !history {
		m, err := scanMemory(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM memories WHERE id = ?", id))
		if err != nil {
			return nil, err
		}
		return []memory.Memory{m}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE history(id, supersedes, depth) AS (
		    SELECT id, supersedes, 0 FROM memories WHERE id = ?
		    UNION ALL
		    SELECT m.id, m.supersedes, h.depth + 1
		    FROM memories m JOIN history h ON m.id = h.supersedes
		)
		SELECT `+columns+` FROM memories
		WHERE id IN (SELECT id FROM history)
		ORDER BY id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, memory.ErrNotFound
	}
	return items, nil
}

func (s *Store) Promote(ctx context.Context, id int, by string, at time.Time, scopeCap int) (memory.Memory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Memory{}, err
	}
	defer func() { _ = tx.Rollback() }()
	m, err := scanMemory(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM memories WHERE id = ? AND "+current, id, at.UnixMicro()))
	if err != nil {
		return memory.Memory{}, err
	}
	if m.Kind == memory.KindNone || m.Provenance != memory.Reported {
		return memory.Memory{}, fmt.Errorf("%w: only reported notes can be promoted", memory.ErrInvalid)
	}
	m.Scope, m.Provenance, m.PromotedBy = memory.SharedScope, memory.Promoted, by
	m.CreatedAt, m.ExpiresAt = at, nil
	stored, err := put(ctx, tx, m, scopeCap)
	if err != nil {
		return memory.Memory{}, err
	}
	return stored, tx.Commit()
}

func (s *Store) Expire(ctx context.Context, id int, actor memory.Actor, at time.Time) (memory.Memory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Memory{}, err
	}
	defer func() { _ = tx.Rollback() }()
	m, err := scanMemory(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM memories WHERE id = ?", id))
	if err != nil {
		return memory.Memory{}, err
	}
	if !memory.CanExpire(m, actor) {
		return memory.Memory{}, memory.ErrForbidden
	}
	if m.ExpiresAt == nil || m.ExpiresAt.After(at) {
		if _, err := tx.ExecContext(ctx, "UPDATE memories SET expires_at = ? WHERE id = ?", at.UnixMicro(), id); err != nil {
			return memory.Memory{}, err
		}
		expiry := at.UTC().Truncate(time.Microsecond)
		m.ExpiresAt = &expiry
	}
	return m, tx.Commit()
}

func (s *Store) Purge(ctx context.Context, id int) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return memory.ErrNotFound
	}
	return err
}

func (s *Store) Reap(ctx context.Context, before time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE expires_at <= ?", before.UnixMicro()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_searches WHERE search_day < ?", time.Now().UTC().Add(-memory.StatsWindow).Format(time.DateOnly)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Health(ctx context.Context) error {
	var available bool
	return s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memories)").Scan(&available)
}
