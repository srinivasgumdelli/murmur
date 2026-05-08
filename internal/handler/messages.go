package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type postMessageRequest struct {
	Sender   string          `json:"sender"`
	Channel  string          `json:"channel"`
	To       *string         `json:"to"`
	Message  string          `json:"message"`
	Metadata json.RawMessage `json:"metadata"`
}

type listMessagesResponse struct {
	Messages []model.Message `json:"messages"`
	LastID   int             `json:"last_id"`
}

func Messages(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postMessage(pool, w, r)
		case http.MethodGet:
			getMessages(pool, w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func postMessage(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request) {
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

	var msg model.Message
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
	_ = json.NewEncoder(w).Encode(msg)
}

func getMessages(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request) {
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

	var msgs []model.Message
	var lastID int
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.ID, &m.Sender, &m.Channel, &m.To, &m.Message, &m.Metadata, &m.CreatedAt); err != nil {
			log.Printf("scan message: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		msgs = append(msgs, m)
		lastID = m.ID
	}
	if msgs == nil {
		msgs = []model.Message{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listMessagesResponse{Messages: msgs, LastID: lastID})
}
