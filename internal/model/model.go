package model

import (
	"encoding/json"
	"time"
)

type Message struct {
	ID        int             `json:"id"`
	Sender    string          `json:"sender"`
	SessionID *string         `json:"session_id,omitempty"`
	Channel   string          `json:"channel"`
	To        *string         `json:"to"`
	ReplyTo   *int            `json:"reply_to,omitempty"`
	Message   string          `json:"message"`
	Metadata  json.RawMessage `json:"metadata"`
	Status    string          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
}

type Agent struct {
	Name         string    `json:"name"`
	SessionID    string    `json:"session_id"`
	Role         string    `json:"role"`
	Description  *string   `json:"description,omitempty"`
	Capabilities []string  `json:"capabilities"`
	Groups       []string  `json:"groups"`
	Status       string    `json:"status"`
	LastSeen     time.Time `json:"last_seen"`
}
