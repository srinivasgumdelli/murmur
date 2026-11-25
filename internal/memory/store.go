package memory

import (
	"context"
	"time"
)

// Store implements persistence for the memory service. Put, Promote and Expire
// perform their read-check-write operations atomically.
type Store interface {
	Put(ctx context.Context, m Memory, scopeCap int) (Memory, error)
	Get(ctx context.Context, id int, history bool) ([]Memory, error)
	Search(ctx context.Context, q Query, at time.Time) (Result, error)
	Promote(ctx context.Context, id int, by string, at time.Time, scopeCap int) (Memory, error)
	Expire(ctx context.Context, id int, actor Actor, at time.Time) (Memory, error)
	Purge(ctx context.Context, id int) error
	Reap(ctx context.Context, before time.Time) error
	Health(ctx context.Context) error
	Stats(ctx context.Context, at time.Time) (Stats, error)
	RecordSearch(ctx context.Context, actor Actor, sessionID string, at time.Time) error
}
