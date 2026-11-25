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
    key TEXT,
    kind TEXT NOT NULL DEFAULT 'note',
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
CREATE INDEX IF NOT EXISTS idx_memories_scope_key ON memories (scope, key, id);
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
id, scope, COALESCE(key, ''), kind, body, tags, author,
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
	fields := []any{&m.ID, &m.Scope, &m.Key, &m.Kind, &m.Body, &m.Tags, &m.Author,
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

func (s *Store) Put(ctx context.Context, m memory.Memory, scopeCap int) (memory.Memory, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return memory.Memory{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockScope(ctx, tx, m.Scope); err != nil {
		return memory.Memory{}, err
	}
	stored, err := put(ctx, tx, m, scopeCap)
	if err != nil {
		return memory.Memory{}, err
	}
	return stored, tx.Commit(ctx)
}

func put(ctx context.Context, tx pgx.Tx, m memory.Memory, scopeCap int) (memory.Memory, error) {
	var replacesCurrent bool
	m.Supersedes = nil
	if m.Key != "" {
		var previous int
		err := tx.QueryRow(ctx, `
			SELECT id, superseded_at IS NULL AND (expires_at IS NULL OR expires_at > $3)
			FROM memories WHERE scope=$1 AND key=$2 ORDER BY id DESC LIMIT 1 FOR UPDATE`,
			m.Scope, m.Key, m.CreatedAt).Scan(&previous, &replacesCurrent)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return memory.Memory{}, err
		}
		if err == nil {
			m.Supersedes = &previous
		}
	}
	if scopeCap > 0 && m.Kind != memory.KindNone && !replacesCurrent {
		var current int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM memories
			WHERE scope=$1 AND kind<>'none' AND superseded_at IS NULL
			AND (expires_at IS NULL OR expires_at > $2)`, m.Scope, m.CreatedAt).Scan(&current)
		if err != nil {
			return memory.Memory{}, err
		}
		if current >= scopeCap {
			return memory.Memory{}, memory.ErrScopeFull
		}
	}
	if m.Supersedes != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE memories SET superseded_at=COALESCE(superseded_at,$2) WHERE id=$1`,
			*m.Supersedes, m.CreatedAt); err != nil {
			return memory.Memory{}, err
		}
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	return scanMemory(tx.QueryRow(ctx, `
		INSERT INTO memories (scope,key,kind,body,tags,author,session_id,provenance,promoted_by,supersedes,created_at,expires_at)
		VALUES ($1,NULLIF($2,''),$3,$4,$5,$6,NULLIF($7,''),$8,NULLIF($9,''),$10,$11,$12)
		RETURNING `+columns, m.Scope, m.Key, m.Kind, m.Body, m.Tags, m.Author, m.SessionID,
		m.Provenance, m.PromotedBy, m.Supersedes, m.CreatedAt, m.ExpiresAt))
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

func (s *Store) Promote(ctx context.Context, id int, by string, at time.Time, scopeCap int) (memory.Memory, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return memory.Memory{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockScope(ctx, tx, memory.SharedScope); err != nil {
		return memory.Memory{}, err
	}
	m, err := scanMemory(tx.QueryRow(ctx, "SELECT "+columns+` FROM memories
		WHERE id=$1 AND superseded_at IS NULL AND (expires_at IS NULL OR expires_at > $2) FOR UPDATE`, id, at))
	if err != nil {
		return memory.Memory{}, err
	}
	if m.Kind == memory.KindNone || m.Provenance == memory.Promoted {
		return memory.Memory{}, fmt.Errorf("%w: only reported notes can be promoted", memory.ErrInvalid)
	}
	m.Scope = memory.SharedScope
	m.Provenance = memory.Promoted
	m.PromotedBy = by
	m.CreatedAt = at
	m.ExpiresAt = nil
	stored, err := put(ctx, tx, m, scopeCap)
	if err != nil {
		return memory.Memory{}, err
	}
	return stored, tx.Commit(ctx)
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

func (s *Store) Purge(ctx context.Context, id int) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM memories WHERE id=$1", id)
	if err == nil && tag.RowsAffected() == 0 {
		return memory.ErrNotFound
	}
	return err
}

func (s *Store) Reap(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM memories WHERE expires_at <= $1", before); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM memory_searches WHERE search_day < $1::date", time.Now().UTC().Add(-memory.StatsWindow).Format(time.DateOnly))
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
		AND (expires_at IS NULL OR expires_at > $1) AND kind <> 'none' GROUP BY scope`, at)
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
