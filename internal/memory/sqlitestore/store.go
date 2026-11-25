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
	if err := migrate(ctx, tx); err != nil {
		return nil, fmt.Errorf("migrate SQLite memory schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []struct {
		name, alterSQL string
	}{
		{"origin_scope", "ALTER TABLE memories ADD COLUMN origin_scope TEXT NOT NULL DEFAULT ''"},
		{"source", "ALTER TABLE memories ADD COLUMN source TEXT NOT NULL DEFAULT 'manual'"},
	} {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM pragma_table_info('memories') WHERE name = ?)`, column.name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, column.alterSQL); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_memories_origin_key
		ON memories(scope, key, origin_scope, id DESC)`)
	return err
}

const columns = `id, scope, origin_scope, COALESCE(key, ''), kind, source, body, tags, author,
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
		&m.ID, &m.Scope, &m.OriginScope, &m.Key, &m.Kind, &m.Source, &m.Body, &tags, &m.Author,
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

func (s *Store) Put(ctx context.Context, m memory.Memory, scopeCap int, actor memory.Actor) (memory.Memory, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Memory{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, created, err := put(ctx, tx, m, scopeCap, actor)
	if err != nil {
		return memory.Memory{}, false, err
	}
	return stored, created, tx.Commit()
}

func put(ctx context.Context, tx *sql.Tx, m memory.Memory, scopeCap int, actor memory.Actor) (memory.Memory, bool, error) {
	m.Supersedes = nil
	at := m.CreatedAt.UnixMicro()
	var replacesCurrent bool
	if m.Key != "" {
		previous, err := scanMemory(tx.QueryRowContext(ctx, "SELECT "+columns+", "+current+` FROM memories
			WHERE scope = ? AND key = ? AND origin_scope = ? ORDER BY id DESC LIMIT 1`,
			at, m.Scope, m.Key, m.OriginScope), &replacesCurrent)
		if err != nil && !errors.Is(err, memory.ErrNotFound) {
			return memory.Memory{}, false, err
		}
		if err == nil {
			if replacesCurrent && !memory.CanExpire(previous, actor) {
				return memory.Memory{}, false, memory.ErrForbidden
			}
			if replacesCurrent && memory.SameCapture(previous, m) {
				return previous, false, nil
			}
			m.Supersedes = &previous.ID
		}
	}
	if scopeCap > 0 && !replacesCurrent {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM memories WHERE scope = ? AND `+current,
			m.Scope, at).Scan(&count); err != nil {
			return memory.Memory{}, false, err
		}
		if count >= scopeCap {
			return memory.Memory{}, false, memory.ErrScopeFull
		}
	}
	if m.Supersedes != nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE memories SET superseded_at = COALESCE(superseded_at, ?) WHERE id = ?`,
			at, *m.Supersedes); err != nil {
			return memory.Memory{}, false, err
		}
	}

	if m.Tags == nil {
		m.Tags = []string{}
	}
	tags, err := json.Marshal(m.Tags)
	if err != nil {
		return memory.Memory{}, false, err
	}
	var expires any
	if m.ExpiresAt != nil {
		expires = m.ExpiresAt.UnixMicro()
	}
	stored, err := scanMemory(tx.QueryRowContext(ctx, `
		INSERT INTO memories(scope, origin_scope, key, kind, source, body, tags, author, session_id,
		                     provenance, promoted_by, supersedes, created_at, expires_at)
		VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?, ?)
		RETURNING `+columns,
		m.Scope, m.OriginScope, m.Key, m.Kind, m.Source, m.Body, string(tags), m.Author, m.SessionID,
		m.Provenance, m.PromotedBy, m.Supersedes, at, expires))
	return stored, err == nil, err
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
	m.OriginScope, m.Scope, m.Provenance, m.PromotedBy = m.Scope, memory.SharedScope, memory.Promoted, by
	m.CreatedAt, m.ExpiresAt = at, nil
	stored, _, err := put(ctx, tx, m, scopeCap, memory.Actor{Lead: true})
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

func (s *Store) Purge(ctx context.Context, id int) (memory.Memory, error) {
	return scanMemory(s.db.QueryRowContext(ctx, "DELETE FROM memories WHERE id = ? RETURNING "+columns, id))
}

func (s *Store) Reap(ctx context.Context, at time.Time, retention time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := at.Add(-retention).UnixMicro()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM memories WHERE expires_at <= ? OR (provenance = ? AND superseded_at <= ?)`,
		cutoff, memory.Promoted, cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_searches WHERE search_day < ?", at.UTC().Add(-memory.StatsWindow).Format(time.DateOnly)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Health(ctx context.Context) error {
	var available bool
	return s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memories)").Scan(&available)
}
