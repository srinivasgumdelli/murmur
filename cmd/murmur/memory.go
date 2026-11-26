package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/memory"
	"github.com/srinivasgumdelli/murmur/internal/memory/pgstore"
)

func openMemoryStore(ctx context.Context, pool *pgxpool.Pool) (memory.Store, func(), error) {
	store := pgstore.New(pool)
	if err := store.Apply(ctx); err != nil {
		return nil, nil, fmt.Errorf("apply Postgres memory schema: %w", err)
	}
	return store, nil, nil
}
