package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/memory"
	"github.com/srinivasgumdelli/murmur/internal/memory/pgstore"
	"github.com/srinivasgumdelli/murmur/internal/memory/sqlitestore"
)

type startupMemoryStore interface {
	memory.Store
	Authors(context.Context) ([]string, error)
}

func openMemoryStore(ctx context.Context, pool *pgxpool.Pool) (startupMemoryStore, func(), error) {
	switch backend := getenv("MURMUR_MEMORY_STORE", "postgres"); backend {
	case "postgres":
		store := pgstore.New(pool)
		if err := store.Apply(ctx); err != nil {
			return nil, nil, fmt.Errorf("apply Postgres memory schema: %w", err)
		}
		return store, nil, nil
	case "sqlite":
		store, err := sqlitestore.Open(ctx, getenv("MURMUR_MEMORY_DB", "./murmur-memory.db"))
		if err != nil {
			return nil, nil, fmt.Errorf("open SQLite memory store: %w", err)
		}
		return store, func() { _ = store.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unsupported MURMUR_MEMORY_STORE %q", backend)
	}
}
