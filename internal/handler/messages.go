package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

// messageRecipient identifies a direct or group recipient in $2.
const messageRecipient = `"to" = $2
	OR "to" IN (SELECT '@' || unnest(groups) FROM agents WHERE name = $2)`

// Logged-in agents see broadcasts and messages they sent or received. Empty
// viewers are operators and unauthenticated chat requests.
const messageVisibility = `($2 = '' OR sender = $2 OR "to" IS NULL OR ` + messageRecipient + `)`

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
	Inbox    *Inbox          `json:"inbox,omitempty"`
}

type postMessageResponse struct {
	model.Message
	Inbox *Inbox `json:"inbox,omitempty"`
}

func Messages(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/messages")
		path = strings.TrimPrefix(path, "/")

		if path == "poll" || path == "stream" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		switch {
		case path == "" && r.Method == http.MethodPost:
			postMessage(pool, w, r)
		case path == "" && r.Method == http.MethodGet:
			getMessages(pool, w, r)
		case strings.HasSuffix(path, "/ack") && r.Method == http.MethodPost:
			ackMessage(pool, w, r, strings.TrimSuffix(path, "/ack"))
		case path != "" && !strings.Contains(path, "/") && r.Method == http.MethodGet:
			getMessage(pool, w, r, path)
		default:
			http.Error(w, "not found", http.StatusNotFound)
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
	if !checkAgent(w, r, req.Sender) {
		return
	}
	if req.Channel == "" {
		req.Channel = "general"
	}
	if req.Metadata == nil {
		req.Metadata = json.RawMessage(`{}`)
	}

	// Auto-register agent on first message if not already registered
	var sessionID string
	if err := pool.QueryRow(r.Context(),
		`INSERT INTO agents (name, session_id, role, capabilities, groups, status, last_seen)
		 VALUES ($1, gen_random_uuid()::text, 'auto', '{}', '{}', 'online', now())
		 ON CONFLICT (name) DO UPDATE SET status = 'online', last_seen = now()
		 RETURNING session_id`,
		req.Sender,
	).Scan(&sessionID); err != nil {
		log.Printf("auto-register agent %s: %v", req.Sender, err)
	}
	if req.SessionID == nil || *req.SessionID == "" {
		req.SessionID = &sessionID
	}

	var msg model.Message
	err := pool.QueryRow(r.Context(),
		`INSERT INTO messages (sender, session_id, channel, "to", reply_to, message, metadata, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'sent')
		 RETURNING id, sender, session_id, channel, "to", reply_to, message, metadata, status, created_at`,
		req.Sender, req.SessionID, req.Channel, req.To, req.ReplyTo, req.Message, req.Metadata,
	).Scan(&msg.ID, &msg.Sender, &msg.SessionID, &msg.Channel, &msg.To, &msg.ReplyTo, &msg.Message, &msg.Metadata, &msg.Status, &msg.CreatedAt)
	if err != nil {
		log.Printf("insert message: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	inbox := fetchInbox(r.Context(), pool, req.Sender)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(postMessageResponse{Message: msg, Inbox: inbox})
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

	direction := "ASC"
	if r.URL.Query().Get("order") == "desc" {
		direction = "DESC"
	}
	rows, err := pool.Query(r.Context(),
		`SELECT id, sender, session_id, channel, "to", reply_to, message, metadata, status, created_at
		 FROM messages WHERE id > $1 AND `+messageVisibility+`
		 AND ($3 = '' OR channel = $3) ORDER BY id `+direction+` LIMIT $4`,
		after, AgentFromContext(r.Context()), channel, limit,
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
		if err := rows.Scan(&m.ID, &m.Sender, &m.SessionID, &m.Channel, &m.To, &m.ReplyTo, &m.Message, &m.Metadata, &m.Status, &m.CreatedAt); err != nil {
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

func getMessage(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	var msg model.Message
	err = pool.QueryRow(r.Context(),
		`SELECT id, sender, session_id, channel, "to", reply_to, message, metadata, status, created_at
		 FROM messages WHERE id = $1 AND `+messageVisibility, id, AgentFromContext(r.Context()),
	).Scan(&msg.ID, &msg.Sender, &msg.SessionID, &msg.Channel, &msg.To, &msg.ReplyTo, &msg.Message, &msg.Metadata, &msg.Status, &msg.CreatedAt)
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}

type ackRequest struct {
	Agent string `json:"agent"`
}

func ackMessage(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	var req ackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Agent == "" {
		http.Error(w, "agent is required", http.StatusBadRequest)
		return
	}

	if !checkAgent(w, r, req.Agent) {
		return
	}

	var msg model.Message
	err = pool.QueryRow(r.Context(),
		`UPDATE messages SET status = 'acked'
		 WHERE id = $1 AND ($2 = '' OR "to" IS NULL OR `+messageRecipient+`)
		 RETURNING id, sender, session_id, channel, "to", reply_to, message, metadata, status, created_at`,
		id, AgentFromContext(r.Context()),
	).Scan(&msg.ID, &msg.Sender, &msg.SessionID, &msg.Channel, &msg.To, &msg.ReplyTo, &msg.Message, &msg.Metadata, &msg.Status, &msg.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("ack message: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}
