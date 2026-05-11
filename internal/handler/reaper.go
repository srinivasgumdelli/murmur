package handler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func StartReaper(ctx context.Context, pool *pgxpool.Pool, messageTTL string) {
	go func() {
		agentTicker := time.NewTicker(1 * time.Minute)
		messageTicker := time.NewTicker(1 * time.Hour)
		defer agentTicker.Stop()
		defer messageTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-agentTicker.C:
				res, err := pool.Exec(ctx,
					`UPDATE agents SET status = 'offline'
					 WHERE status = 'online' AND last_seen < now() - interval '5 minutes'`)
				if err != nil {
					log.Printf("reaper: %v", err)
					continue
				}
				if res.RowsAffected() > 0 {
					log.Printf("reaper: marked %d agent(s) offline", res.RowsAffected())
				}
			case <-messageTicker.C:
				if messageTTL == "" {
					continue
				}
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
