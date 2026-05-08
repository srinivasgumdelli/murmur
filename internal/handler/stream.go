package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type notifyPayload struct {
	ID      int     `json:"id"`
	Channel string  `json:"channel"`
	To      *string `json:"to"`
}

func Stream(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		channel := r.URL.Query().Get("channel")
		if !r.URL.Query().Has("channel") {
			channel = "general"
		}
		agent := r.URL.Query().Get("agent")

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		conn, err := pool.Acquire(r.Context())
		if err != nil {
			log.Printf("acquire conn for listen: %v", err)
			return
		}
		defer conn.Release()

		if _, err := conn.Exec(r.Context(), "LISTEN new_message"); err != nil {
			log.Printf("listen: %v", err)
			return
		}

		heartbeat := time.NewTicker(30 * time.Second)
		defer heartbeat.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-heartbeat.C:
				_, _ = fmt.Fprintf(w, "event: heartbeat\ndata: {}\n\n")
				flusher.Flush()
			default:
			}

			notification, err := conn.Conn().WaitForNotification(r.Context())
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				log.Printf("wait notification: %v", err)
				return
			}

			var payload notifyPayload
			if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil {
				log.Printf("unmarshal notification: %v", err)
				continue
			}

			if channel != "" && payload.Channel != channel {
				continue
			}
			if agent != "" && payload.To != nil && *payload.To != agent {
				continue
			}

			var msg model.Message
			err = pool.QueryRow(r.Context(),
				`UPDATE messages SET status = CASE WHEN status = 'sent' THEN 'delivered' ELSE status END
				 WHERE id = $1
				 RETURNING id, sender, session_id, channel, "to", reply_to, message, metadata, status, created_at`,
				payload.ID,
			).Scan(&msg.ID, &msg.Sender, &msg.SessionID, &msg.Channel, &msg.To, &msg.ReplyTo, &msg.Message, &msg.Metadata, &msg.Status, &msg.CreatedAt)
			if err != nil {
				log.Printf("fetch message %d: %v", payload.ID, err)
				continue
			}

			data, _ := json.Marshal(msg)
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
