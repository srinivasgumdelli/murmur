package handler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func StartReaper(ctx context.Context, pool *pgxpool.Pool, messageTTL string, agentTTL string) {
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
				rows, err := pool.Query(ctx,
					`UPDATE agents SET status = 'offline'
					 WHERE status = 'online' AND last_seen < now() - interval '3 minutes'
					 RETURNING name`)
				if err != nil {
					log.Printf("reaper: %v", err)
					continue
				}
				var count int
				for rows.Next() {
					var name string
					if err := rows.Scan(&name); err == nil {
						postSystemMessage(ctx, pool, name+" went offline")
						count++
					}
				}
				rows.Close()
				if count > 0 {
					log.Printf("reaper: marked %d agent(s) offline", count)
				}

				removed, err := pool.Query(ctx,
					fmt.Sprintf(`DELETE FROM agents WHERE status = 'offline' AND last_seen < now() - interval '%s' RETURNING name`, agentTTL))
				if err != nil {
					log.Printf("reaper: agent cleanup: %v", err)
				} else {
					var rCount int
					for removed.Next() {
						var name string
						if err := removed.Scan(&name); err == nil {
							postSystemMessage(ctx, pool, name+" removed (inactive)")
							rCount++
						}
					}
					removed.Close()
					if rCount > 0 {
						log.Printf("reaper: removed %d stale agent(s)", rCount)
					}
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
