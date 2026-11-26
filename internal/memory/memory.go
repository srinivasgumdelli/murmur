package memory

import (
	"errors"
	"slices"
	"time"
)

type Kind string

const (
	KindNote     Kind = "note"
	KindCommand  Kind = "command"
	KindDecision Kind = "decision"
	KindPitfall  Kind = "pitfall"
	KindNone     Kind = "none"
	KindAny      Kind = "any"
)

type Provenance string

const (
	Reported    Provenance = "reported"
	Promoted    Provenance = "promoted"
	SharedScope            = "shared"
	AdminAuthor            = "@murmur-admin"
)

type Source string

const (
	SourceManual  Source = "manual"
	SourceCapture Source = "capture"
)

var (
	ErrInvalid      = errors.New("invalid memory request")
	ErrAuthRequired = errors.New("memory requires MURMUR_AUTH=required")
	ErrUnauthorized = errors.New("memory requires a valid API key")
	ErrForbidden    = errors.New("memory operation forbidden")
	ErrNotFound     = errors.New("memory not found")
	ErrScopeFull    = errors.New("memory scope is full; expire or consolidate entries")
	ErrBodyTooLarge = errors.New("memory body exceeds 4096 bytes")
)

type Memory struct {
	ID          int        `json:"id"`
	Scope       string     `json:"scope"`
	OriginScope string     `json:"origin_scope,omitempty"`
	OriginID    *int       `json:"origin_id,omitempty"`
	Key         string     `json:"key,omitempty"`
	Kind        Kind       `json:"kind"`
	Source      Source     `json:"source"`
	Body        string     `json:"body"`
	Tags        []string   `json:"tags"`
	Author      string     `json:"author"`
	SessionID   string     `json:"session_id,omitempty"`
	Provenance  Provenance `json:"provenance"`
	PromotedBy  string     `json:"promoted_by,omitempty"`
	Supersedes  *int       `json:"supersedes,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type Actor struct {
	Agent string
	Admin bool
	Lead  bool
}

func (a Actor) Name() string {
	if a.Admin {
		return AdminAuthor
	}
	return a.Agent
}

type PutRequest struct {
	Scope     string   `json:"scope"`
	Key       string   `json:"key,omitempty"`
	Kind      Kind     `json:"kind,omitempty"`
	Body      string   `json:"body,omitempty"`
	Source    Source   `json:"source,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	TTLDays   *int     `json:"ttl_days,omitempty"`
}

type Query struct {
	Scopes          []string
	Text            string
	Tags            []string
	Kind            Kind
	Source          Source
	Author          string
	SessionID       string
	SearchSessionID string
	Since           time.Time
	IncludeHistory  bool
	Limit           int
}

type Result struct {
	Items []Memory `json:"items"`
	Total int      `json:"total"`
}

type Stats struct {
	CountsPerScope     map[string]int `json:"counts_per_scope"`
	WritesPerAgent     map[string]int `json:"writes_per_agent"`
	SearchesPerAgent   map[string]int `json:"searches_per_agent"`
	SearchesPerSession map[string]int `json:"searches_per_session"`
	NoneShare          float64        `json:"none_share"`
}

const StatsWindow = 7 * 24 * time.Hour

type Event struct {
	ID          int        `json:"id"`
	Action      string     `json:"action"`
	Scope       string     `json:"scope"`
	OriginScope string     `json:"origin_scope,omitempty"`
	OriginID    *int       `json:"origin_id,omitempty"`
	Key         string     `json:"key"`
	Kind        Kind       `json:"kind"`
	Provenance  Provenance `json:"provenance"`
}

func CanExpire(m Memory, actor Actor) bool {
	return actor.Admin || actor.Lead || (m.Provenance == Reported && m.Author == actor.Agent)
}

// ReuseCapture coalesces short repeats but still refreshes a recurring
// command's expiry by creating a new revision at most once per day.
func ReuseCapture(previous, next Memory) bool {
	if previous.Source != SourceCapture || next.Source != SourceCapture ||
		next.Provenance != Reported || previous.Author != next.Author || previous.Body != next.Body ||
		!slices.Equal(previous.Tags, next.Tags) {
		return false
	}
	if previous.ExpiresAt == nil || next.ExpiresAt == nil {
		return previous.ExpiresAt == nil && next.ExpiresAt == nil
	}
	ttl := next.ExpiresAt.Sub(next.CreatedAt)
	return ttl == previous.ExpiresAt.Sub(previous.CreatedAt) &&
		next.ExpiresAt.Sub(*previous.ExpiresAt) < min(24*time.Hour, ttl/4)
}
