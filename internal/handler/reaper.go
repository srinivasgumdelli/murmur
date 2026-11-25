package handler

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func StartReaper(ctx context.Context, pool *pgxpool.Pool, messageTTL string, reapMemory func(context.Context) error) {
	if messageTTL == "" && reapMemory == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if reapMemory != nil {
					if err := reapMemory(ctx); err != nil {
						log.Printf("reaper: memory cleanup: %v", err)
					}
				}
				if messageTTL == "" {
					continue
				}
				res, err := pool.Exec(ctx,
					`DELETE FROM messages WHERE created_at < now() - $1::interval`, messageTTL)
				if err != nil {
					log.Printf("reaper: message cleanup: %v", err)
					continue
				}
				if res.RowsAffected() > 0 {
					log.Printf("reaper: deleted %d expired message(s)", res.RowsAffected())
				}
			}
		}
	}()
}
