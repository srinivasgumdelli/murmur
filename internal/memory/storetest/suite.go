// Package storetest defines the behavioral contract shared by all memory stores.
package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/memory"
)

func Run(t *testing.T, newStore func(*testing.T) memory.Store) {
	t.Helper()
	now := time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)

	t.Run("writes and history", func(t *testing.T) {
		ctx := t.Context()
		store := newStore(t)
		first := put(t, store, entry("workspace:app", "cmd.test", "Run unit tests", now), 1)
		next := put(t, store, entry("workspace:app", "cmd.test", "Run integration tests", now.Add(time.Second)), 1)
		if first.Author != "worker" || first.Provenance != memory.Reported || first.ID == next.ID ||
			next.Supersedes == nil || *next.Supersedes != first.ID {
			t.Fatalf("incorrect stored revisions: %+v, %+v", first, next)
		}
		chain, err := store.Get(ctx, next.ID, true)
		if err != nil || len(chain) != 2 || chain[0].ID != next.ID || chain[1].ID != first.ID {
			t.Fatalf("history = %+v, err = %v", chain, err)
		}
		if _, err := store.Put(ctx, entry(next.Scope, "another", "another note", now), 1); !errors.Is(err, memory.ErrScopeFull) {
			t.Fatalf("over-cap write: %v", err)
		}
		actor := memory.Actor{Agent: "worker"}
		if _, err := store.Expire(ctx, next.ID, actor, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		find(t, store, memory.Query{}, now.Add(3*time.Second), 0)
		find(t, store, memory.Query{IncludeHistory: true}, now.Add(3*time.Second), 2)
		if err := store.Purge(ctx, next.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Get(ctx, next.ID, false); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("purged entry: %v", err)
		}
		find(t, store, memory.Query{}, now.Add(3*time.Second), 0)
	})

	t.Run("promotion", func(t *testing.T) {
		ctx := t.Context()
		store := newStore(t)
		claim := put(t, store, entry("task:deploy", "cmd.deploy", "deploy production", now), 1)
		promoted, err := store.Promote(ctx, claim.ID, "lead", now.Add(time.Second), 1)
		if err != nil || promoted.Scope != memory.SharedScope || promoted.Author != claim.Author ||
			promoted.Provenance != memory.Promoted || promoted.PromotedBy != "lead" || promoted.ExpiresAt != nil {
			t.Fatalf("promotion = %+v, err = %v", promoted, err)
		}
		next, err := store.Promote(ctx, claim.ID, "lead", now.Add(2*time.Second), 1)
		if err != nil || next.Supersedes == nil || *next.Supersedes != promoted.ID {
			t.Fatalf("replacement promotion = %+v, err = %v", next, err)
		}
		if _, err := store.Expire(ctx, next.ID, memory.Actor{Agent: "worker"}, now); !errors.Is(err, memory.ErrForbidden) {
			t.Fatalf("author expiring promoted entry: %v", err)
		}
		found := find(t, store, memory.Query{Text: "production", Limit: 1}, now.Add(3*time.Second), 2)
		if len(found.Items) != 1 || found.Items[0].ID != next.ID {
			t.Fatalf("ranking or limit = %+v", found)
		}
	})

	t.Run("filters and retention", func(t *testing.T) {
		ctx := t.Context()
		store := newStore(t)
		note := entry("workspace:app", "", "compile app", now)
		note.Tags = []string{"build", "go"}
		note.Kind = memory.KindCommand
		note.SessionID = "test-session"
		expires := now.Add(time.Hour)
		note.ExpiresAt = &expires
		saved := put(t, store, note, 1)
		none := entry(note.Scope, "", "", now.Add(time.Second))
		none.Kind = memory.KindNone
		put(t, store, none, 1)

		find(t, store, memory.Query{}, now.Add(time.Minute), 1)
		find(t, store, memory.Query{Kind: memory.KindAny}, now.Add(time.Minute), 2)
		find(t, store, memory.Query{Text: "compile", Scopes: []string{"workspace:app", "task:other"},
			Tags: []string{"go", "build"}, Author: "worker", SessionID: "test-session", Kind: memory.KindCommand,
			Since: now}, now.Add(time.Minute), 1)
		find(t, store, memory.Query{Tags: []string{"other"}}, now.Add(time.Minute), 0)
		find(t, store, memory.Query{Since: now.Add(time.Nanosecond * 1000)}, now.Add(time.Minute), 0)
		find(t, store, memory.Query{}, expires, 0)
		find(t, store, memory.Query{IncludeHistory: true}, expires, 1)

		if err := store.RecordSearch(ctx, memory.Actor{Agent: "worker"}, "test-session", now); err != nil {
			t.Fatal(err)
		}
		stats, err := store.Stats(ctx, now.Add(time.Minute))
		if err != nil || stats.CountsPerScope[note.Scope] != 1 || stats.WritesPerAgent["worker"] != 2 ||
			stats.NoneShare != 0.5 || stats.SearchesPerAgent["worker"] != 1 || stats.SearchesPerSession["test-session"] != 1 {
			t.Fatalf("stats = %+v, err = %v", stats, err)
		}
		if err := store.Reap(ctx, expires); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Get(ctx, saved.ID, false); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("reaped entry: %v", err)
		}
		if err := store.Health(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("concurrent keyed writes", func(t *testing.T) {
		store := newStore(t)
		const writes = 10
		results := make(chan error, writes)
		for range writes {
			go func() {
				_, err := store.Put(t.Context(), entry("workspace:app", "cmd.test", "run tests", now), 1)
				results <- err
			}()
		}
		for range writes {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
		find(t, store, memory.Query{}, now, 1)
		find(t, store, memory.Query{IncludeHistory: true}, now, writes)
	})

	t.Run("concurrent cap", func(t *testing.T) {
		store := newStore(t)
		const writes = 10
		results := make(chan error, writes)
		for range writes {
			go func() {
				_, err := store.Put(t.Context(), entry("workspace:app", "", "run tests", now), 2)
				results <- err
			}()
		}
		var accepted int
		for range writes {
			err := <-results
			if err == nil {
				accepted++
			} else if !errors.Is(err, memory.ErrScopeFull) {
				t.Error(err)
			}
		}
		if accepted != 2 {
			t.Errorf("accepted %d writes, want 2", accepted)
		}
		find(t, store, memory.Query{}, now, 2)
	})
}

func entry(scope, key, body string, at time.Time) memory.Memory {
	return memory.Memory{Scope: scope, Key: key, Body: body, Author: "worker",
		Kind: memory.KindNote, Provenance: memory.Reported, CreatedAt: at}
}

func put(t *testing.T, store memory.Store, m memory.Memory, cap int) memory.Memory {
	t.Helper()
	stored, err := store.Put(t.Context(), m, cap)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func find(t *testing.T, store memory.Store, query memory.Query, at time.Time, want int) memory.Result {
	t.Helper()
	result, err := store.Search(t.Context(), query, at)
	if err != nil || result.Total != want {
		t.Fatalf("search %+v = %+v, err = %v, want %d results", query, result, err, want)
	}
	return result
}
