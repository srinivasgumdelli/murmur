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
		if _, _, err := store.Put(ctx, entry(next.Scope, "another", "another note", now), 1, memory.Actor{Agent: "worker"}); !errors.Is(err, memory.ErrScopeFull) {
			t.Fatalf("over-cap write: %v", err)
		}
		actor := memory.Actor{Agent: "worker"}
		if _, err := store.Expire(ctx, next.ID, actor, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		find(t, store, memory.Query{}, now.Add(3*time.Second), 0)
		find(t, store, memory.Query{IncludeHistory: true}, now.Add(3*time.Second), 2)
		if deleted, err := store.Purge(ctx, next.ID); err != nil || deleted.ID != next.ID || deleted.Scope != next.Scope {
			t.Fatalf("purged revision = %+v, err = %v", deleted, err)
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
		promoted, created, err := store.Promote(ctx, claim.ID, "lead", now.Add(time.Second), 1)
		if err != nil || !created || promoted.OriginID == nil || *promoted.OriginID != claim.ID ||
			promoted.Scope != memory.SharedScope || promoted.OriginScope != claim.Scope || promoted.Author != claim.Author ||
			promoted.Provenance != memory.Promoted || promoted.PromotedBy != "lead" || promoted.ExpiresAt != nil {
			t.Fatalf("promotion = %+v, err = %v", promoted, err)
		}
		next, created, err := store.Promote(ctx, claim.ID, "lead", now.Add(2*time.Second), 1)
		if err != nil || !created || next.Supersedes == nil || *next.Supersedes != promoted.ID {
			t.Fatalf("replacement promotion = %+v, err = %v", next, err)
		}
		if _, err := store.Expire(ctx, next.ID, memory.Actor{Agent: "worker"}, now); !errors.Is(err, memory.ErrForbidden) {
			t.Fatalf("author expiring promoted entry: %v", err)
		}
		found := find(t, store, memory.Query{Text: "production", Limit: 1}, now.Add(3*time.Second), 2)
		if len(found.Items) != 1 || found.Items[0].ID != next.ID {
			t.Fatalf("ranking or limit = %+v", found)
		}

		other := put(t, store, entry("workspace:web", claim.Key, "deploy web", now), 1)
		alsoPromoted, created, err := store.Promote(ctx, other.ID, "lead", now.Add(3*time.Second), 2)
		if err != nil || !created || alsoPromoted.Key != claim.Key || alsoPromoted.OriginScope != other.Scope || alsoPromoted.Supersedes != nil {
			t.Fatalf("promotion from another scope = %+v, err = %v", alsoPromoted, err)
		}
		find(t, store, memory.Query{Scopes: []string{memory.SharedScope}}, now.Add(4*time.Second), 2)
		find(t, store, memory.Query{Scopes: []string{memory.SharedScope}, IncludeHistory: true}, now.Add(4*time.Second), 3)

		retention := 30 * 24 * time.Hour
		if err := store.Reap(ctx, now.Add(2*time.Second).Add(retention), retention); err != nil {
			t.Fatal(err)
		}
		history := find(t, store, memory.Query{Scopes: []string{memory.SharedScope}, IncludeHistory: true}, now.Add(4*time.Second), 2)
		for _, item := range history.Items {
			if item.ID == promoted.ID || item.Supersedes != nil {
				t.Errorf("superseded promotion survived reap: %+v", item)
			}
		}
	})

	t.Run("unkeyed promotion is idempotent", func(t *testing.T) {
		ctx := t.Context()
		store := newStore(t)
		claim := put(t, store, entry("workspace:app", "", "first note", now), 2)
		first, created, err := store.Promote(ctx, claim.ID, "lead", now, 1)
		if err != nil || !created || first.OriginID == nil || *first.OriginID != claim.ID {
			t.Fatalf("first promotion = %+v, created = %v, err = %v", first, created, err)
		}
		// Origin expiry should not create a second copy or change the result of a retry.
		if _, err := store.Expire(ctx, claim.ID, memory.Actor{Agent: claim.Author}, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		retry, created, err := store.Promote(ctx, claim.ID, "lead", now.Add(2*time.Second), 1)
		if err != nil || created || retry.ID != first.ID {
			t.Fatalf("repeated promotion = %+v, created = %v, err = %v", retry, created, err)
		}
		other := put(t, store, entry(claim.Scope, "", "second note", now), 2)
		if _, _, err := store.Promote(ctx, other.ID, "lead", now.Add(2*time.Second), 1); !errors.Is(err, memory.ErrScopeFull) {
			t.Fatalf("different note promotion: got %v, want scope full", err)
		}
		find(t, store, memory.Query{Scopes: []string{memory.SharedScope}, IncludeHistory: true}, now.Add(2*time.Second), 1)
	})

	t.Run("natural-language search", func(t *testing.T) {
		store := newStore(t)
		put(t, store, entry("workspace:app", "", "Run tests with make test.", now), 2)
		find(t, store, memory.Query{Text: "How do I run the tests?"}, now, 1)
		find(t, store, memory.Query{Text: "How do I run the integration tests?"}, now, 0)
		find(t, store, memory.Query{Text: "How do I?"}, now, 0)
		put(t, store, entry("workspace:app", "", "Deploy staging with make deploy.", now), 2)
		find(t, store, memory.Query{Text: "how to deploy from staging before noon"}, now, 0)
		find(t, store, memory.Query{Text: "how to deploy from staging, after that again"}, now, 1)
		find(t, store, memory.Query{Text: "what about deploying from staging, once more?"}, now, 1)
	})

	t.Run("none markers skip the scope cap", func(t *testing.T) {
		ctx := t.Context()
		store := newStore(t)
		put(t, store, entry("workspace:cap", "", "only note", now), 1)
		for i := range 3 {
			none := entry("workspace:cap", "", "", now.Add(time.Duration(i+1)*time.Second))
			none.Kind = memory.KindNone
			put(t, store, none, 1)
		}
		if _, _, err := store.Put(ctx, entry("workspace:cap", "", "second note", now.Add(5*time.Second)), 1, memory.Actor{Agent: "worker"}); !errors.Is(err, memory.ErrScopeFull) {
			t.Fatalf("over-cap note: %v, want scope full", err)
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
		saved := put(t, store, note, 2)
		none := entry(note.Scope, "", "", now.Add(time.Second))
		none.Kind = memory.KindNone
		put(t, store, none, 2)

		find(t, store, memory.Query{Limit: 1}, now.Add(time.Minute), 1)
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
		if err != nil || stats.CountsPerScope[note.Scope] != 2 || stats.WritesPerAgent["worker"] != 2 ||
			stats.NoneShare != 0.5 || stats.SearchesPerAgent["worker"] != 1 || stats.SearchesPerSession["test-session"] != 1 {
			t.Fatalf("stats = %+v, err = %v", stats, err)
		}
		if err := store.Reap(ctx, expires, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Get(ctx, saved.ID, false); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("reaped entry: %v", err)
		}
		if err := store.Health(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("key ownership", func(t *testing.T) {
		store := newStore(t)
		ctx := t.Context()
		owner := entry("workspace:app", "cmd.test", "run owner's tests", now)
		other := owner
		other.Author, other.Body = "other", "run other tests"
		type write struct {
			item memory.Memory
			err  error
		}
		results := make(chan write, 2)
		for _, note := range []memory.Memory{owner, other} {
			go func() {
				item, _, err := store.Put(ctx, note, 1, memory.Actor{Agent: note.Author})
				results <- write{item, err}
			}()
		}
		var winner memory.Memory
		var denied int
		for range 2 {
			r := <-results
			switch {
			case r.err == nil:
				winner = r.item
			case errors.Is(r.err, memory.ErrForbidden):
				denied++
			default:
				t.Error(r.err)
			}
		}
		if denied != 1 || winner.ID == 0 {
			t.Fatalf("concurrent writers: winner = %+v, denied = %d", winner, denied)
		}
		find(t, store, memory.Query{IncludeHistory: true}, now, 1)
		for i, actor := range []memory.Actor{{Agent: "lead", Lead: true}, {Admin: true}} {
			takeover := entry(owner.Scope, owner.Key, "replace as privileged agent", now.Add(time.Duration(i+1)*time.Second))
			takeover.Author = actor.Name()
			replacement, created, err := store.Put(ctx, takeover, 1, actor)
			if err != nil || !created || replacement.Supersedes == nil || *replacement.Supersedes != winner.ID {
				t.Fatalf("privileged replacement = %+v, created = %v, err = %v", replacement, created, err)
			}
			winner = replacement
		}
	})

	t.Run("repeated captured command", func(t *testing.T) {
		store := newStore(t)
		capture := entry("workspace:app", "cmd.test", "go test", now)
		capture.Kind, capture.Source = memory.KindCommand, memory.SourceCapture
		const ttl = 90 * 24 * time.Hour
		firstExpiry := now.Add(ttl)
		capture.ExpiresAt = &firstExpiry
		first := put(t, store, capture, 3)
		manual := entry(capture.Scope, "", "make test", now)
		manual.Kind, manual.SessionID = memory.KindCommand, "session"
		put(t, store, manual, 3)
		capture.CreatedAt = now.Add(time.Second)
		retryExpiry := capture.CreatedAt.Add(ttl)
		capture.ExpiresAt = &retryExpiry
		capture.SessionID = manual.SessionID
		duplicate, created, err := store.Put(t.Context(), capture, 3, memory.Actor{Agent: capture.Author})
		if err != nil || created || duplicate.ID != first.ID || duplicate.Source != memory.SourceCapture {
			t.Fatalf("duplicate capture = %+v, created = %v, err = %v", duplicate, created, err)
		}
		capture.CreatedAt = now.Add(24 * time.Hour)
		renewedExpiry := capture.CreatedAt.Add(ttl)
		capture.ExpiresAt = &renewedExpiry
		renewed := put(t, store, capture, 3)
		if renewed.ID == first.ID || renewed.Supersedes == nil || *renewed.Supersedes != first.ID ||
			renewed.ExpiresAt == nil || !renewed.ExpiresAt.Equal(renewedExpiry) {
			t.Fatalf("renewed capture = %+v, want newer revision and fresh expiry", renewed)
		}
		find(t, store, memory.Query{Source: memory.SourceCapture}, firstExpiry, 1)
		capture.Body = "go test ./..."
		put(t, store, capture, 3)
		find(t, store, memory.Query{Source: memory.SourceCapture, IncludeHistory: true}, capture.CreatedAt, 3)
		find(t, store, memory.Query{Source: memory.SourceManual, SessionID: manual.SessionID}, capture.CreatedAt, 1)

	})

	t.Run("search metric retention follows reaper time", func(t *testing.T) {
		store := newStore(t)
		at := time.Date(2000, time.January, 1, 12, 0, 0, 0, time.UTC)
		actor := memory.Actor{Agent: "worker"}
		if err := store.RecordSearch(t.Context(), actor, "session", at); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			reapAt time.Time
			want   int
		}{
			{at.Add(memory.StatsWindow), 1},
			{at.Add(memory.StatsWindow + 24*time.Hour), 0},
		} {
			if err := store.Reap(t.Context(), test.reapAt, 0); err != nil {
				t.Fatal(err)
			}
			stats, err := store.Stats(t.Context(), at)
			if err != nil || stats.SearchesPerSession["session"] != test.want {
				t.Fatalf("stats = %+v, err = %v, want %d searches", stats, err, test.want)
			}
		}
	})

	t.Run("concurrent keyed writes", func(t *testing.T) {
		store := newStore(t)
		const writes = 10
		results := make(chan error, writes)
		for range writes {
			go func() {
				_, _, err := store.Put(t.Context(), entry("workspace:app", "cmd.test", "run tests", now), 1, memory.Actor{Agent: "worker"})
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
				_, _, err := store.Put(t.Context(), entry("workspace:app", "", "run tests", now), 2, memory.Actor{Agent: "worker"})
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
		Kind: memory.KindNote, Source: memory.SourceManual, Provenance: memory.Reported, CreatedAt: at}
}

func put(t *testing.T, store memory.Store, m memory.Memory, cap int) memory.Memory {
	t.Helper()
	stored, created, err := store.Put(t.Context(), m, cap, memory.Actor{Agent: m.Author})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("put returned an existing entry")
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
