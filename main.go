package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id SERIAL PRIMARY KEY,
    sender TEXT NOT NULL,
    channel TEXT NOT NULL DEFAULT 'general',
    "to" TEXT,
    message TEXT NOT NULL,
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages (channel, id);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages (created_at);

CREATE TABLE IF NOT EXISTS agents (
    name TEXT PRIMARY KEY,
    role TEXT NOT NULL,
    capabilities TEXT[] DEFAULT '{}',
    last_seen TIMESTAMPTZ DEFAULT now()
);

CREATE OR REPLACE FUNCTION notify_new_message()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('new_message', json_build_object(
        'id', NEW.id,
        'channel', NEW.channel,
        'to', NEW."to"
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS message_inserted ON messages;
CREATE TRIGGER message_inserted
    AFTER INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION notify_new_message();
`

type Message struct {
	ID        int              `json:"id"`
	Sender    string           `json:"sender"`
	Channel   string           `json:"channel"`
	To        *string          `json:"to"`
	Message   string           `json:"message"`
	Metadata  json.RawMessage  `json:"metadata"`
	CreatedAt time.Time        `json:"created_at"`
}

type postMessageRequest struct {
	Sender   string          `json:"sender"`
	Channel  string          `json:"channel"`
	To       *string         `json:"to"`
	Message  string          `json:"message"`
	Metadata json.RawMessage `json:"metadata"`
}

type listMessagesResponse struct {
	Messages []Message `json:"messages"`
	LastID   int       `json:"last_id"`
}

func handlePostMessage(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req postMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Sender == "" || req.Message == "" {
			http.Error(w, "sender and message are required", http.StatusBadRequest)
			return
		}
		if req.Channel == "" {
			req.Channel = "general"
		}
		if req.Metadata == nil {
			req.Metadata = json.RawMessage(`{}`)
		}

		var msg Message
		err := pool.QueryRow(r.Context(),
			`INSERT INTO messages (sender, channel, "to", message, metadata)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id, sender, channel, "to", message, metadata, created_at`,
			req.Sender, req.Channel, req.To, req.Message, req.Metadata,
		).Scan(&msg.ID, &msg.Sender, &msg.Channel, &msg.To, &msg.Message, &msg.Metadata, &msg.CreatedAt)
		if err != nil {
			log.Printf("insert message: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(msg)
	}
}

func handleGetMessages(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		channel := r.URL.Query().Get("channel")
		if channel == "" {
			channel = "general"
		}
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 200 {
			limit = 50
		}

		rows, err := pool.Query(r.Context(),
			`SELECT id, sender, channel, "to", message, metadata, created_at
			 FROM messages
			 WHERE channel = $1 AND id > $2
			 ORDER BY id ASC
			 LIMIT $3`,
			channel, after, limit,
		)
		if err != nil {
			log.Printf("query messages: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var msgs []Message
		var lastID int
		for rows.Next() {
			var m Message
			if err := rows.Scan(&m.ID, &m.Sender, &m.Channel, &m.To, &m.Message, &m.Metadata, &m.CreatedAt); err != nil {
				log.Printf("scan message: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			msgs = append(msgs, m)
			lastID = m.ID
		}
		if msgs == nil {
			msgs = []Message{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(listMessagesResponse{Messages: msgs, LastID: lastID})
	}
}

type notifyPayload struct {
	ID      int     `json:"id"`
	Channel string  `json:"channel"`
	To      *string `json:"to"`
}

func handleStream(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		channel := r.URL.Query().Get("channel")
		if channel == "" {
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
				fmt.Fprintf(w, "event: heartbeat\ndata: {}\n\n")
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

			if payload.Channel != channel {
				continue
			}
			if agent != "" && payload.To != nil && *payload.To != agent {
				continue
			}

			var msg Message
			err = pool.QueryRow(r.Context(),
				`SELECT id, sender, channel, "to", message, metadata, created_at
				 FROM messages WHERE id = $1`, payload.ID,
			).Scan(&msg.ID, &msg.Sender, &msg.Channel, &msg.To, &msg.Message, &msg.Metadata, &msg.CreatedAt)
			if err != nil {
				log.Printf("fetch message %d: %v", payload.ID, err)
				continue
			}

			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	dbURL := getenv("BUS_DATABASE_URL", "postgres://bus:bus@localhost:5432/bus?sslmode=disable")
	port := getenv("BUS_PORT", "4444")

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, schema); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	log.Printf("schema applied")

	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handlePostMessage(pool)(w, r)
		case http.MethodGet:
			handleGetMessages(pool)(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/messages/stream", handleStream(pool))

	log.Printf("agentic-bus ready on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}
