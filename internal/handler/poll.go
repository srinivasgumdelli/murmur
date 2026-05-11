package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type pollResponse struct {
	Messages []model.Message `json:"messages"`
	LastID   int             `json:"last_id"`
}

func Poll(pool *pgxpool.Pool, notifier *Notifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := r.URL.Query().Get("agent")
		if agent == "" {
			http.Error(w, "agent param is required", http.StatusBadRequest)
			return
		}

		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		timeoutSec, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
		if timeoutSec <= 0 || timeoutSec > 60 {
			timeoutSec = 30
		}

		// Update agent heartbeat
		_, _ = pool.Exec(r.Context(),
			`UPDATE agents SET status = 'online', last_seen = now() WHERE name = $1`, agent)

		// Check for existing messages first
		msgs, lastID := fetchPollMessages(r.Context(), pool, agent, after)
		if len(msgs) > 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pollResponse{Messages: msgs, LastID: lastID})
			return
		}

		// No messages — wait for notification or timeout
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSec)*time.Second)
		defer cancel()

		notifier.Wait(ctx, agent)

		// Refresh heartbeat after wait
		_, _ = pool.Exec(r.Context(),
			`UPDATE agents SET status = 'online', last_seen = now() WHERE name = $1`, agent)

		// Check again after wakeup
		msgs, lastID = fetchPollMessages(r.Context(), pool, agent, after)
		if msgs == nil {
			msgs = []model.Message{}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pollResponse{Messages: msgs, LastID: lastID})
	}
}

func fetchPollMessages(ctx context.Context, pool *pgxpool.Pool, agent string, after int) ([]model.Message, int) {
	rows, err := pool.Query(ctx,
		`SELECT id, sender, session_id, channel, "to", reply_to, message, metadata, status, created_at
		 FROM messages
		 WHERE id > $1
		 AND (
		     sender = $2
		     OR "to" = $2
		     OR "to" IN (SELECT '@' || unnest(groups) FROM agents WHERE name = $2)
		     OR ("to" IS NULL AND reply_to IS NULL)
		     OR ("to" IS NULL AND reply_to IS NOT NULL AND EXISTS (
		         SELECT 1 FROM messages AS parent
		         WHERE parent.id = messages.reply_to AND parent.sender = $2
		     ))
		     OR ("to" IS NULL AND reply_to IS NOT NULL AND EXISTS (
		         SELECT 1 FROM messages AS sibling
		         WHERE sibling.reply_to = messages.reply_to AND sibling.sender = $2
		     ))
		 )
		 ORDER BY id ASC
		 LIMIT 50`,
		after, agent,
	)
	if err != nil {
		log.Printf("poll query: %v", err)
		return nil, 0
	}
	defer rows.Close()

	var msgs []model.Message
	var lastID int
	var ids []int
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.ID, &m.Sender, &m.SessionID, &m.Channel, &m.To, &m.ReplyTo, &m.Message, &m.Metadata, &m.Status, &m.CreatedAt); err != nil {
			log.Printf("poll scan: %v", err)
			continue
		}
		if m.Status == "sent" {
			ids = append(ids, m.ID)
			m.Status = "delivered"
		}
		msgs = append(msgs, m)
		lastID = m.ID
	}

	if len(ids) > 0 {
		_, _ = pool.Exec(ctx,
			`UPDATE messages SET status = 'delivered' WHERE id = ANY($1::int[])`, ids)
	}

	return msgs, lastID
}
