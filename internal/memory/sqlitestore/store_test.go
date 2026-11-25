package sqlitestore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/memory"
	"github.com/srinivasgumdelli/murmur/internal/memory/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) memory.Store {
		s, err := Open(t.Context(), filepath.Join(t.TempDir(), "memory.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestFTSLiteralQueries(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	at := time.Now().UTC()
	_, err = s.Put(ctx, memory.Memory{
		Scope: "workspace:sample", Body: "Run tests using make test before changes.", Author: "worker",
		Kind: memory.KindCommand, Provenance: memory.Reported, CreatedAt: at,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{`"`, `*`, `-`, `NEAR(`, `foo-not"bar*`, `"bad" NOT "query"`} {
		t.Run(query, func(t *testing.T) {
			if _, err := s.Search(t.Context(), memory.Query{Text: query}, at); err != nil {
				t.Fatalf("literal search failed: %v", err)
			}
		})
	}
	result, err := s.Search(ctx, memory.Query{Text: "make test"}, at)
	if err != nil || result.Total != 1 {
		t.Fatalf("search returned (%+v, %v), want one result", result, err)
	}
}

func TestSearchIndexAfterPurgeAndReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "memory #1?.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	old, err := s.Put(ctx, memory.Memory{
		Scope: "workspace:sample", Key: "cmd.test", Body: "previous instruction", Author: "worker",
		Kind: memory.KindCommand, Provenance: memory.Reported, CreatedAt: at,
	}, 1)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	replacement := old
	replacement.Body = "new instruction"
	newer, err := s.Put(ctx, replacement, 1)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Purge(ctx, newer.ID); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	result, err := s.Search(ctx, memory.Query{Text: "instruction"}, at)
	if err != nil || result.Total != 0 {
		t.Fatalf("active search after purge returned (%+v, %v), want neither revision", result, err)
	}
	result, err = s.Search(ctx, memory.Query{Text: "instruction", IncludeHistory: true}, at)
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != old.ID {
		t.Fatalf("history search after purge returned (%+v, %v), want revision %d", result, err, old.ID)
	}
	if _, err := s.Get(ctx, newer.ID, false); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("get purged revision returned %v", err)
	}
}
