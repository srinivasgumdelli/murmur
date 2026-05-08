package model

import (
	"encoding/json"
	"time"
)

type Message struct {
	ID        int             `json:"id"`
	Sender    string          `json:"sender"`
	Channel   string          `json:"channel"`
	To        *string         `json:"to"`
	Message   string          `json:"message"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

type Agent struct {
	Name         string    `json:"name"`
	Role         string    `json:"role"`
	Capabilities []string  `json:"capabilities"`
	LastSeen     time.Time `json:"last_seen"`
}
