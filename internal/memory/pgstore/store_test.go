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
	storetest.Run(t, func(t *testing.T) memory.Store {
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
	})
}
