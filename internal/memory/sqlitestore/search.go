package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/memory"
)

const current = "superseded_at IS NULL AND (expires_at IS NULL OR expires_at > ?)"

func (s *Store) Search(ctx context.Context, q memory.Query, at time.Time) (memory.Result, error) {
	var args []any
	var rowsFilter []string
	var preamble, join, rankOrder string
	if text := ftsQuery(q.Text); text != "" {
		// Materialize the scores while FTS5 has MATCH's cursor context.
		preamble = `WITH hits AS MATERIALIZED (
		    SELECT rowid AS memory_id, bm25(memories_fts) AS score
		    FROM memories_fts WHERE memories_fts MATCH ?
		) `
		args = append(args, text)
		join = " JOIN hits ON hits.memory_id = memories.id"
		rankOrder = "score ASC, "
	}
	if !q.IncludeHistory {
		rowsFilter = append(rowsFilter, current)
		args = append(args, at.UnixMicro())
	}
	if len(q.Scopes) != 0 {
		placeholders := make([]string, 0, len(q.Scopes))
		for _, scope := range q.Scopes {
			placeholders = append(placeholders, "?")
			args = append(args, scope)
		}
		rowsFilter = append(rowsFilter, "scope IN ("+strings.Join(placeholders, ", ")+")")
	}
	for _, tag := range q.Tags {
		rowsFilter = append(rowsFilter, "EXISTS(SELECT 1 FROM json_each(memories.tags) WHERE value = ?)")
		args = append(args, tag)
	}
	switch q.Kind {
	case "":
		rowsFilter = append(rowsFilter, "kind != ?")
		args = append(args, memory.KindNone)
	case memory.KindAny:
	default:
		rowsFilter = append(rowsFilter, "kind = ?")
		args = append(args, q.Kind)
	}
	if q.Source != "" {
		rowsFilter = append(rowsFilter, "source = ?")
		args = append(args, q.Source)
	}
	if q.Author != "" {
		rowsFilter = append(rowsFilter, "author = ?")
		args = append(args, q.Author)
	}
	if q.SessionID != "" {
		rowsFilter = append(rowsFilter, "session_id = ?")
		args = append(args, q.SessionID)
	}
	if !q.Since.IsZero() {
		rowsFilter = append(rowsFilter, "created_at >= ?")
		args = append(args, q.Since.UnixMicro())
	}
	where := ""
	if len(rowsFilter) != 0 {
		where = " WHERE " + strings.Join(rowsFilter, " AND ")
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	query := preamble + "SELECT " + columns + ", COUNT(*) OVER() FROM memories" + join + where +
		" ORDER BY " + rankOrder + "(provenance = ?) DESC, created_at DESC, id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, append(args, memory.Promoted, q.Limit)...)
	if err != nil {
		return memory.Result{}, err
	}
	defer rows.Close()
	out := memory.Result{Items: []memory.Memory{}}
	for rows.Next() {
		m, err := scanMemory(rows, &out.Total)
		if err != nil {
			return memory.Result{}, err
		}
		out.Items = append(out.Items, m)
	}
	return out, rows.Err()
}

// Quote each term so input is interpreted as search text. The SQLite FTS5
// parser still tokenizes punctuation within the quotes.
func ftsQuery(text string) string {
	words := strings.Fields(text)
	for i, word := range words {
		words[i] = `"` + strings.ReplaceAll(word, `"`, `""`) + `"`
	}
	return strings.Join(words, " AND ")
}

func (s *Store) RecordSearch(ctx context.Context, actor memory.Actor, sessionID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_searches(author, session_id, search_day, count) VALUES (?, ?, ?, 1)
		ON CONFLICT (author, session_id, search_day) DO UPDATE SET count = memory_searches.count + 1`,
		actor.Name(), sessionID, at.UTC().Format(time.DateOnly))
	return err
}

func (s *Store) Stats(ctx context.Context, at time.Time) (memory.Stats, error) {
	var out memory.Stats
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	since := at.Add(-memory.StatsWindow)
	out.CountsPerScope, err = counts(ctx, tx,
		"SELECT scope, COUNT(*) FROM memories WHERE "+current+" GROUP BY scope", at.UnixMicro())
	if err != nil {
		return memory.Stats{}, err
	}
	out.WritesPerAgent, err = counts(ctx, tx,
		"SELECT author, COUNT(*) FROM memories WHERE created_at >= ? GROUP BY author", since.UnixMicro())
	if err != nil {
		return memory.Stats{}, err
	}
	out.SearchesPerAgent, err = counts(ctx, tx,
		"SELECT author, SUM(count) FROM memory_searches WHERE search_day >= ? GROUP BY author", since.UTC().Format(time.DateOnly))
	if err != nil {
		return memory.Stats{}, err
	}
	out.SearchesPerSession, err = counts(ctx, tx,
		"SELECT session_id, SUM(count) FROM memory_searches WHERE search_day >= ? AND session_id != '' GROUP BY session_id", since.UTC().Format(time.DateOnly))
	if err != nil {
		return memory.Stats{}, err
	}
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(avg(kind = ?), 0)
		FROM memories WHERE created_at >= ?`, memory.KindNone, since.UnixMicro()).Scan(&out.NoneShare)
	if err != nil {
		return memory.Stats{}, err
	}
	return out, tx.Commit()
}

func counts(ctx context.Context, tx *sql.Tx, query string, value any) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx, query, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("scan SQLite memory stats: %w", err)
		}
		out[name] = count
	}
	return out, rows.Err()
}
