package memory

import (
	"context"
	"time"
)

// Store implements persistence for the memory service. Put, Promote and Expire
// perform their read-check-write operations atomically.
type Store interface {
	// Put returns whether a new revision was stored. A short repeat of the same
	// capture returns the current revision without creating an additional row.
	Put(ctx context.Context, m Memory, scopeCap int, actor Actor) (Memory, bool, error)
	Get(ctx context.Context, id int, history bool) ([]Memory, error)
	Search(ctx context.Context, q Query, at time.Time) (Result, error)
	// Promote reuses an active promotion for the same unkeyed source entry.
	// The boolean reports whether a new shared revision was stored.
	Promote(ctx context.Context, id int, by string, at time.Time, scopeCap int) (Memory, bool, error)
	Expire(ctx context.Context, id int, actor Actor, at time.Time) (Memory, error)
	Purge(ctx context.Context, id int) (Memory, error)
	Reap(ctx context.Context, at time.Time, retention time.Duration) error
	Health(ctx context.Context) error
	Stats(ctx context.Context, at time.Time) (Stats, error)
	RecordSearch(ctx context.Context, actor Actor, sessionID string, at time.Time) error
}
