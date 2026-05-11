package handler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func StartReaper(ctx context.Context, pool *pgxpool.Pool, messageTTL string) {
	if messageTTL == "" {
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
				res, err := pool.Exec(ctx,
					fmt.Sprintf(`DELETE FROM messages WHERE created_at < now() - interval '%s'`, messageTTL))
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
