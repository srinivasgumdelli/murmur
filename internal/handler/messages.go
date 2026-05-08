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
	Sender    string          `json:"sender"`
	SessionID *string         `json:"session_id"`
	Channel   string          `json:"channel"`
	To        *string         `json:"to"`
	ReplyTo   *int            `json:"reply_to"`
	Message   string          `json:"message"`
	Metadata  json.RawMessage `json:"metadata"`
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
		`INSERT INTO messages (sender, session_id, channel, "to", reply_to, message, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, sender, session_id, channel, "to", reply_to, message, metadata, created_at`,
		req.Sender, req.SessionID, req.Channel, req.To, req.ReplyTo, req.Message, req.Metadata,
	).Scan(&msg.ID, &msg.Sender, &msg.SessionID, &msg.Channel, &msg.To, &msg.ReplyTo, &msg.Message, &msg.Metadata, &msg.CreatedAt)
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
	if !r.URL.Query().Has("channel") {
		channel = "general"
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var query string
	var args []any
	if channel == "" {
		query = `SELECT id, sender, session_id, channel, "to", reply_to, message, metadata, created_at
		 FROM messages WHERE id > $1 ORDER BY id ASC LIMIT $2`
		args = []any{after, limit}
	} else {
		query = `SELECT id, sender, session_id, channel, "to", reply_to, message, metadata, created_at
		 FROM messages WHERE channel = $1 AND id > $2 ORDER BY id ASC LIMIT $3`
		args = []any{channel, after, limit}
	}

	rows, err := pool.Query(r.Context(), query, args...)
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
		if err := rows.Scan(&m.ID, &m.Sender, &m.SessionID, &m.Channel, &m.To, &m.ReplyTo, &m.Message, &m.Metadata, &m.CreatedAt); err != nil {
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
