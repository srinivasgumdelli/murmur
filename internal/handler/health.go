package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Health(pool *pgxpool.Pool, startTime time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msgCount int
		var agentCount int
		_ = pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM messages").Scan(&msgCount)
		_ = pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM agents").Scan(&agentCount)

		uptime := time.Since(startTime).Round(time.Second)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"messages": msgCount,
			"agents":   agentCount,
			"uptime":   uptime.String(),
		})
	}
}
