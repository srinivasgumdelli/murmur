package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/memory"
)

const schema = `
CREATE TABLE IF NOT EXISTS memories (
    id SERIAL PRIMARY KEY,
    scope TEXT NOT NULL,
    origin_scope TEXT NOT NULL DEFAULT '',
    origin_id INT,
    key TEXT,
    kind TEXT NOT NULL DEFAULT 'note',
    source TEXT NOT NULL DEFAULT 'manual',
    body TEXT NOT NULL,
    tags TEXT[] NOT NULL DEFAULT '{}',
    author TEXT NOT NULL,
    session_id TEXT,
    provenance TEXT NOT NULL DEFAULT 'reported',
    promoted_by TEXT,
    supersedes INT REFERENCES memories(id) ON DELETE SET NULL,
    superseded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    body_tsv TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', body)) STORED
);
ALTER TABLE memories ADD COLUMN IF NOT EXISTS origin_scope TEXT NOT NULL DEFAULT '';
ALTER TABLE memories ADD COLUMN IF NOT EXISTS origin_id INT;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'manual';
CREATE INDEX IF NOT EXISTS idx_memories_origin_key ON memories (scope, key, origin_scope, id);
CREATE INDEX IF NOT EXISTS idx_memories_unkeyed_origin ON memories (scope, origin_id, id DESC)
    WHERE key IS NULL AND origin_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memories_author_created ON memories (author, created_at);
CREATE INDEX IF NOT EXISTS idx_memories_tsv ON memories USING GIN (body_tsv);
CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memories_supersedes ON memories (supersedes) WHERE supersedes IS NOT NULL;

CREATE TABLE IF NOT EXISTS memory_searches (
    author TEXT NOT NULL,
    session_id TEXT NOT NULL,
    search_day DATE NOT NULL,
    count INT NOT NULL,
    PRIMARY KEY (author, session_id, search_day)
);
CREATE INDEX IF NOT EXISTS idx_memory_searches_day ON memory_searches (search_day);
`

const columns = `
id, scope, origin_scope, origin_id, COALESCE(key, ''), kind, source, body, tags, author,
COALESCE(session_id, ''), provenance, COALESCE(promoted_by, ''),
supersedes, created_at, expires_at`

type Store struct {
	pool *pgxpool.Pool
}

var _ memory.Store = (*Store)(nil)

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Apply(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schema)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMemory(row scanner, tail ...any) (memory.Memory, error) {
	var m memory.Memory
	fields := []any{&m.ID, &m.Scope, &m.OriginScope, &m.OriginID, &m.Key, &m.Kind, &m.Source, &m.Body, &m.Tags, &m.Author,
		&m.SessionID, &m.Provenance, &m.PromotedBy, &m.Supersedes, &m.CreatedAt, &m.ExpiresAt}
	err := row.Scan(append(fields, tail...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = memory.ErrNotFound
	}
	return m, err
}

func lockScope(ctx context.Context, tx pgx.Tx, scope string) error {
	// A scope lock covers the cap as well as first writes to a key, where
	// there is no row to lock yet.
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 41736))", scope)
	return err
}

func (s *Store) Put(ctx context.Context, m memory.Memory, scopeCap int, actor memory.Actor) (memory.Memory, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return memory.Memory{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockScope(ctx, tx, m.Scope); err != nil {
		return memory.Memory{}, false, err
	}
	stored, created, err := put(ctx, tx, m, scopeCap, actor)
	if err != nil {
		return memory.Memory{}, false, err
	}
	return stored, created, tx.Commit(ctx)
}

func put(ctx context.Context, tx pgx.Tx, m memory.Memory, scopeCap int, actor memory.Actor) (memory.Memory, bool, error) {
	var replacesCurrent bool
	m.Supersedes = nil
	if m.Key != "" {
		previous, err := scanMemory(tx.QueryRow(ctx, "SELECT "+columns+`,
			superseded_at IS NULL AND (expires_at IS NULL OR expires_at > $4)
			FROM memories WHERE scope=$1 AND key=$2 AND origin_scope=$3
			ORDER BY id DESC LIMIT 1 FOR UPDATE`,
			m.Scope, m.Key, m.OriginScope, m.CreatedAt), &replacesCurrent)
		if err != nil && !errors.Is(err, memory.ErrNotFound) {
			return memory.Memory{}, false, err
		}
		if err == nil {
			if replacesCurrent && !memory.CanExpire(previous, actor) {
				return memory.Memory{}, false, memory.ErrForbidden
			}
			if replacesCurrent && memory.ReuseCapture(previous, m) {
				return previous, false, nil
			}
			m.Supersedes = &previous.ID
		}
	}
	if scopeCap > 0 && !replacesCurrent && m.Kind != memory.KindNone {
		var current int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM memories
			WHERE scope=$1 AND kind<>'none' AND superseded_at IS NULL
			AND (expires_at IS NULL OR expires_at > $2)`, m.Scope, m.CreatedAt).Scan(&current)
		if err != nil {
			return memory.Memory{}, false, err
		}
		if current >= scopeCap {
			return memory.Memory{}, false, memory.ErrScopeFull
		}
	}
	if m.Supersedes != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE memories SET superseded_at=COALESCE(superseded_at,$2) WHERE id=$1`,
			*m.Supersedes, m.CreatedAt); err != nil {
			return memory.Memory{}, false, err
		}
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	stored, err := scanMemory(tx.QueryRow(ctx, `
		INSERT INTO memories (scope,origin_scope,origin_id,key,kind,source,body,tags,author,session_id,provenance,promoted_by,supersedes,created_at,expires_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,NULLIF($10,''),$11,NULLIF($12,''),$13,$14,$15)
		RETURNING `+columns, m.Scope, m.OriginScope, m.OriginID, m.Key, m.Kind, m.Source, m.Body, m.Tags, m.Author, m.SessionID,
		m.Provenance, m.PromotedBy, m.Supersedes, m.CreatedAt, m.ExpiresAt))
	return stored, err == nil, err
}

func (s *Store) Get(ctx context.Context, id int, history bool) ([]memory.Memory, error) {
	if !history {
		m, err := scanMemory(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM memories WHERE id=$1", id))
		if err != nil {
			return nil, err
		}
		return []memory.Memory{m}, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT memories.*, 0 AS depth FROM memories WHERE id=$1
			UNION ALL
			SELECT m.*, c.depth+1 FROM memories m JOIN chain c ON m.id=c.supersedes
		)
		SELECT `+columns+` FROM chain ORDER BY depth`, id)
	if err != nil {
		return nil, err
	}
	items, err := collect(rows)
	if err == nil && len(items) == 0 {
		return nil, memory.ErrNotFound
	}
	return items, err
}

func collect(rows pgx.Rows) ([]memory.Memory, error) {
	defer rows.Close()
	items := []memory.Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

func (s *Store) Search(ctx context.Context, q memory.Query, at time.Time) (memory.Result, error) {
	var args []any
	var clauses []string
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if !q.IncludeHistory {
		clauses = append(clauses, "superseded_at IS NULL AND (expires_at IS NULL OR expires_at > "+addArg(at)+")")
	}
	switch q.Kind {
	case memory.KindAny:
	case "":
		clauses = append(clauses, "kind <> 'none'")
	default:
		clauses = append(clauses, "kind = "+addArg(q.Kind))
	}
	if q.Source != "" {
		clauses = append(clauses, "source = "+addArg(q.Source))
	}
	if len(q.Scopes) > 0 {
		clauses = append(clauses, "scope = ANY("+addArg(q.Scopes)+")")
	}
	if len(q.Tags) > 0 {
		clauses = append(clauses, "tags @> "+addArg(q.Tags))
	}
	if q.Author != "" {
		clauses = append(clauses, "author = "+addArg(q.Author))
	}
	if q.SessionID != "" {
		clauses = append(clauses, "session_id = "+addArg(q.SessionID))
	}
	if !q.Since.IsZero() {
		clauses = append(clauses, "created_at >= "+addArg(q.Since))
	}
	order := "(provenance='promoted') DESC, created_at DESC, id DESC"
	if strings.TrimSpace(q.Text) != "" {
		search := "plainto_tsquery('english', " + addArg(q.Text) + ")"
		clauses = append(clauses, "body_tsv @@ "+search)
		order = "ts_rank(body_tsv, " + search + ") DESC, " + order
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	limit := q.Limit
	if limit < 1 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, "SELECT "+columns+", count(*) OVER() FROM memories"+where+
		" ORDER BY "+order+" LIMIT "+addArg(limit), args...)
	if err != nil {
		return memory.Result{}, err
	}
	defer rows.Close()
	result := memory.Result{Items: []memory.Memory{}}
	for rows.Next() {
		m, err := scanMemory(rows, &result.Total)
		if err != nil {
			return memory.Result{}, err
		}
		result.Items = append(result.Items, m)
	}
	return result, rows.Err()
}

func (s *Store) Promote(ctx context.Context, id int, by string, at time.Time, scopeCap int) (memory.Memory, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return memory.Memory{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockScope(ctx, tx, memory.SharedScope); err != nil {
		return memory.Memory{}, false, err
	}
	// Keep an active promotion for a keyless source idempotent even if the
	// reported entry has already expired. The scope lock also covers retries.
	existing, err := scanMemory(tx.QueryRow(ctx, "SELECT "+columns+` FROM memories
		WHERE scope=$1 AND key IS NULL AND origin_id=$2
		AND superseded_at IS NULL AND (expires_at IS NULL OR expires_at > $3)
		ORDER BY id DESC LIMIT 1 FOR UPDATE`, memory.SharedScope, id, at))
	if err == nil {
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, memory.ErrNotFound) {
		return memory.Memory{}, false, err
	}
	m, err := scanMemory(tx.QueryRow(ctx, "SELECT "+columns+` FROM memories
		WHERE id=$1 AND superseded_at IS NULL AND (expires_at IS NULL OR expires_at > $2) FOR UPDATE`, id, at))
	if err != nil {
		return memory.Memory{}, false, err
	}
	if m.Kind == memory.KindNone || m.Provenance == memory.Promoted {
		return memory.Memory{}, false, fmt.Errorf("%w: only reported notes can be promoted", memory.ErrInvalid)
	}
	m.OriginScope, m.OriginID, m.Scope = m.Scope, &id, memory.SharedScope
	m.Provenance = memory.Promoted
	m.PromotedBy = by
	m.CreatedAt = at
	m.ExpiresAt = nil
	stored, created, err := put(ctx, tx, m, scopeCap, memory.Actor{Lead: true})
	if err != nil {
		return memory.Memory{}, false, err
	}
	return stored, created, tx.Commit(ctx)
}

func (s *Store) Expire(ctx context.Context, id int, actor memory.Actor, at time.Time) (memory.Memory, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return memory.Memory{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	m, err := scanMemory(tx.QueryRow(ctx, "SELECT "+columns+" FROM memories WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return memory.Memory{}, err
	}
	if !memory.CanExpire(m, actor) {
		return memory.Memory{}, memory.ErrForbidden
	}
	m, err = scanMemory(tx.QueryRow(ctx, `
		UPDATE memories SET expires_at=LEAST(expires_at,$2) WHERE id=$1 RETURNING `+columns, id, at))
	if err != nil {
		return memory.Memory{}, err
	}
	return m, tx.Commit(ctx)
}

func (s *Store) Purge(ctx context.Context, id int) (memory.Memory, error) {
	return scanMemory(s.pool.QueryRow(ctx, "DELETE FROM memories WHERE id=$1 RETURNING "+columns, id))
}

func (s *Store) Reap(ctx context.Context, at time.Time, retention time.Duration) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM memories WHERE expires_at <= $1 OR (provenance = $2 AND superseded_at <= $1)`,
		at.Add(-retention), memory.Promoted); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM memory_searches WHERE search_day < $1::date", at.UTC().Add(-memory.StatsWindow).Format(time.DateOnly))
	return err
}

func (s *Store) Health(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "SELECT 1 FROM memories LIMIT 0")
	return err
}

func (s *Store) RecordSearch(ctx context.Context, actor memory.Actor, sessionID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO memory_searches (author, session_id, search_day, count)
		VALUES ($1, $2, $3::date, 1)
		ON CONFLICT (author, session_id, search_day) DO UPDATE SET count=memory_searches.count+1`,
		actor.Name(), sessionID, at.UTC().Format(time.DateOnly))
	return err
}

func (s *Store) Stats(ctx context.Context, at time.Time) (memory.Stats, error) {
	stats := memory.Stats{
		CountsPerScope: map[string]int{}, WritesPerAgent: map[string]int{},
		SearchesPerAgent: map[string]int{}, SearchesPerSession: map[string]int{},
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return stats, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	scopeRows, err := tx.Query(ctx, `
		SELECT scope, count(*) FROM memories WHERE superseded_at IS NULL
		AND (expires_at IS NULL OR expires_at > $1) GROUP BY scope`, at)
	if err != nil {
		return stats, err
	}
	for scopeRows.Next() {
		var scope string
		var count int
		if err := scopeRows.Scan(&scope, &count); err != nil {
			scopeRows.Close()
			return stats, err
		}
		stats.CountsPerScope[scope] = count
	}
	if err := scopeRows.Err(); err != nil {
		return stats, err
	}
	since := at.Add(-memory.StatsWindow)
	writeRows, err := tx.Query(ctx, `
		SELECT author, count(*), count(*) FILTER (WHERE kind='none')
		FROM memories WHERE created_at >= $1 GROUP BY author`, since)
	if err != nil {
		return stats, err
	}
	var totalWrites, noneWrites int
	for writeRows.Next() {
		var author string
		var count, none int
		if err := writeRows.Scan(&author, &count, &none); err != nil {
			writeRows.Close()
			return stats, err
		}
		stats.WritesPerAgent[author] = count
		totalWrites += count
		noneWrites += none
	}
	if err := writeRows.Err(); err != nil {
		return stats, err
	}
	if totalWrites > 0 {
		stats.NoneShare = float64(noneWrites) / float64(totalWrites)
	}
	searchRows, err := tx.Query(ctx, `
		SELECT author, session_id, sum(count) FROM memory_searches
		WHERE search_day >= $1::date GROUP BY author, session_id`, since.UTC().Format(time.DateOnly))
	if err != nil {
		return stats, err
	}
	for searchRows.Next() {
		var author, sessionID string
		var count int
		if err := searchRows.Scan(&author, &sessionID, &count); err != nil {
			searchRows.Close()
			return stats, err
		}
		stats.SearchesPerAgent[author] += count
		if sessionID != "" {
			stats.SearchesPerSession[sessionID] += count
		}
	}
	if err := searchRows.Err(); err != nil {
		return stats, err
	}
	return stats, tx.Commit(ctx)
}

// Authors returns the authors and promoting leads of current and historical entries.
func (s *Store) Authors(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT author FROM memories UNION SELECT promoted_by FROM memories WHERE promoted_by IS NOT NULL AND promoted_by <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var authors []string
	for rows.Next() {
		var author string
		if err := rows.Scan(&author); err != nil {
			return nil, err
		}
		authors = append(authors, author)
	}
	return authors, rows.Err()
}
