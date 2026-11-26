package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Health(pool *pgxpool.Pool, startTime time.Time, memoryCheck func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msgCount int
		var agentCount int
		_ = pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM messages").Scan(&msgCount)
		_ = pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM agents").Scan(&agentCount)

		uptime := time.Since(startTime).Round(time.Second)

		result := map[string]any{
			"status":   "ok",
			"messages": msgCount,
			"agents":   agentCount,
			"uptime":   uptime.String(),
		}
		status := http.StatusOK
		if memoryCheck != nil {
			if err := memoryCheck(r.Context()); err != nil {
				result["status"] = "degraded"
				result["memory"] = "unavailable"
				status = http.StatusServiceUnavailable
			} else {
				result["memory"] = "ok"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(result)
	}
}
