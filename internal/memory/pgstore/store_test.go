package pgstore

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/memory"
	"github.com/srinivasgumdelli/murmur/internal/memory/storetest"
)

func TestStore(t *testing.T) {
	newStore := testStore(t)
	storetest.Run(t, func(t *testing.T) memory.Store { return newStore(t) })
}

func TestApplyPreservesPreviousSchemaEntries(t *testing.T) {
	s := testStore(t)(t)
	ctx := t.Context()
	// Recreate the layout from before the origin and source columns.
	if _, err := s.pool.Exec(ctx, `
		ALTER TABLE memories DROP COLUMN origin_scope, DROP COLUMN origin_id, DROP COLUMN source;
		INSERT INTO memories (scope, key, body, author) VALUES
			('workspace:api', 'cmd.test', 'go test ./...', 'worker');
	`); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search(ctx, memory.Query{Text: "test", Source: memory.SourceManual}, time.Now())
	if err != nil || got.Total != 1 || got.Items[0].Key != "cmd.test" || got.Items[0].OriginScope != "" || got.Items[0].OriginID != nil {
		t.Fatalf("legacy notes = %+v, err = %v", got, err)
	}
}

func testStore(t *testing.T) func(*testing.T) *Store {
	t.Helper()
	url := os.Getenv("MURMUR_TEST_PG_URL")
	if url == "" {
		t.Skip("MURMUR_TEST_PG_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var sequence atomic.Int64
	return func(t *testing.T) *Store {
		t.Helper()
		namespace := fmt.Sprintf("test_memory_%d_%d", time.Now().UnixNano(), sequence.Add(1))
		if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+namespace); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := admin.Exec(ctx, "DROP SCHEMA "+namespace+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		config, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		config.ConnConfig.RuntimeParams["search_path"] = namespace
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		store := New(pool)
		if err := store.Apply(t.Context()); err != nil {
			t.Fatal(err)
		}
		return store
	}
}
