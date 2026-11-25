package memory

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultTTL   = 90
	defaultCap   = 1000
	maxBody      = 4096
	maxTTL       = 36500
	defaultLimit = 20
	maxLimit     = 100
)

var validIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@:/-]{0,127}$`)

type Config struct {
	AuthMode string
	TTLDays  int
	ScopeCap int
	Now      func() time.Time
	Publish  func(Event)
}

type Service struct {
	store Store
	cfg   Config
}

func NewService(store Store, cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TTLDays < 1 || cfg.TTLDays > maxTTL {
		cfg.TTLDays = defaultTTL
	}
	if cfg.ScopeCap < 1 {
		cfg.ScopeCap = defaultCap
	}
	return &Service{store: store, cfg: cfg}
}

func (s *Service) authorize(actor Actor) error {
	if s.cfg.AuthMode != "required" {
		return ErrAuthRequired
	}
	if actor.Name() == "" {
		return ErrUnauthorized
	}
	if !actor.Admin && actor.Agent == AdminAuthor {
		return ErrForbidden
	}
	return nil
}

func invalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, message)
}

func validScope(scope string) bool {
	if scope == SharedScope {
		return true
	}
	prefix, id, ok := strings.Cut(scope, ":")
	return ok && (prefix == "workspace" || prefix == "task") && validIdentifier.MatchString(id)
}

func validKind(kind Kind) bool {
	switch kind {
	case KindNote, KindCommand, KindDecision, KindPitfall, KindNone:
		return true
	}
	return false
}

func validID(id int) bool {
	return id > 0 && id <= math.MaxInt32
}

func validText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validTags(tags []string) bool {
	if len(tags) > 32 {
		return false
	}
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" || !validText(tag, 128) {
			return false
		}
	}
	return true
}

func (s *Service) Put(ctx context.Context, actor Actor, req PutRequest) (Memory, error) {
	if err := s.authorize(actor); err != nil {
		return Memory{}, err
	}
	if !validScope(req.Scope) {
		return Memory{}, invalid("scope must be workspace:<id> or task:<id>")
	}
	if req.Scope == SharedScope {
		return Memory{}, ErrForbidden
	}
	if req.Key != "" && !validIdentifier.MatchString(req.Key) {
		return Memory{}, invalid("invalid key")
	}
	if req.Kind == "" {
		req.Kind = KindNote
	}
	if !validKind(req.Kind) {
		return Memory{}, invalid("unsupported kind")
	}
	if len(req.Body) > maxBody {
		return Memory{}, ErrBodyTooLarge
	}
	if !validText(req.Body, maxBody) || (req.Kind != KindNone && strings.TrimSpace(req.Body) == "") {
		return Memory{}, invalid("body must be valid nonempty text up to 4096 bytes")
	}
	if req.Source == "" {
		req.Source = SourceManual
	}
	switch req.Source {
	case SourceManual:
	case SourceCapture:
		if req.Kind != KindCommand || req.Key == "" {
			return Memory{}, invalid("capture source requires a keyed command")
		}
	default:
		return Memory{}, invalid("unsupported source")
	}
	if req.Kind == KindNone && req.Key != "" {
		return Memory{}, invalid("none entries cannot have a key")
	}
	if !validText(req.SessionID, 128) || !validTags(req.Tags) {
		return Memory{}, invalid("invalid session_id or tags")
	}
	ttl := s.cfg.TTLDays
	if req.TTLDays != nil {
		ttl = *req.TTLDays
		if ttl < 1 || ttl > maxTTL {
			return Memory{}, invalid("ttl_days must be between 1 and 36500")
		}
	}
	now := s.cfg.Now().UTC()
	expiresAt := now.AddDate(0, 0, ttl)
	m := Memory{
		Scope: req.Scope, Key: req.Key, Kind: req.Kind, Body: req.Body, Tags: req.Tags,
		Author: actor.Name(), SessionID: req.SessionID, Provenance: Reported, Source: req.Source,
		CreatedAt: now, ExpiresAt: &expiresAt,
	}
	stored, created, err := s.store.Put(ctx, m, s.cfg.ScopeCap, actor)
	if err == nil && created {
		s.publish("put", stored)
	}
	return stored, err
}

func (s *Service) Search(ctx context.Context, actor Actor, q Query) (Result, error) {
	if err := s.authorize(actor); err != nil {
		return Result{}, err
	}
	if len(q.Scopes) > 100 {
		return Result{}, invalid("at most 100 scopes can be searched at a time")
	}
	for _, scope := range q.Scopes {
		if !validScope(scope) {
			return Result{}, invalid("invalid scope")
		}
	}
	if q.Kind != "" && q.Kind != KindAny && !validKind(q.Kind) {
		return Result{}, invalid("unsupported kind")
	}
	if q.Source != "" && q.Source != SourceManual && q.Source != SourceCapture {
		return Result{}, invalid("unsupported source")
	}
	if !validText(q.Text, 1024) || !validText(q.SessionID, 128) || !validText(q.SearchSessionID, 128) ||
		!validText(q.Author, 512) || !validTags(q.Tags) {
		return Result{}, invalid("invalid search parameters")
	}
	if q.Author == "me" {
		q.Author = actor.Name()
	}
	if q.Limit < 0 || q.Limit > maxLimit {
		return Result{}, invalid("limit must be between 1 and 100")
	}
	if q.Limit == 0 {
		q.Limit = defaultLimit
	}
	now := s.cfg.Now().UTC()
	result, err := s.store.Search(ctx, q, now)
	if err != nil {
		return Result{}, err
	}
	if result.Items == nil {
		result.Items = []Memory{}
	}
	if err := s.store.RecordSearch(ctx, actor, q.SearchSessionID, now); err != nil {
		log.Printf("memory: record search: %v", err)
	}
	return result, nil
}

func (s *Service) Get(ctx context.Context, actor Actor, id int, history bool) ([]Memory, error) {
	if err := s.authorize(actor); err != nil {
		return nil, err
	}
	if !validID(id) {
		return nil, invalid("invalid id")
	}
	return s.store.Get(ctx, id, history)
}

func (s *Service) Promote(ctx context.Context, actor Actor, id int) (Memory, error) {
	if err := s.authorize(actor); err != nil {
		return Memory{}, err
	}
	if !actor.Admin && !actor.Lead {
		return Memory{}, ErrForbidden
	}
	if !validID(id) {
		return Memory{}, invalid("invalid id")
	}
	m, err := s.store.Promote(ctx, id, actor.Name(), s.cfg.Now().UTC(), s.cfg.ScopeCap)
	if err == nil {
		s.publish("promote", m)
	}
	return m, err
}

func (s *Service) Expire(ctx context.Context, actor Actor, id int) error {
	if err := s.authorize(actor); err != nil {
		return err
	}
	if !validID(id) {
		return invalid("invalid id")
	}
	m, err := s.store.Expire(ctx, id, actor, s.cfg.Now().UTC())
	if err == nil {
		s.publish("expire", m)
	}
	return err
}

func (s *Service) Purge(ctx context.Context, actor Actor, id int) error {
	if err := s.authorize(actor); err != nil {
		return err
	}
	if !actor.Admin {
		return ErrForbidden
	}
	if !validID(id) {
		return invalid("invalid id")
	}
	m, err := s.store.Purge(ctx, id)
	if err != nil {
		return err
	}
	log.Printf("memory: admin purged entry id=%d", id)
	s.publish("purge", m)
	return nil
}

func (s *Service) Stats(ctx context.Context, actor Actor) (Stats, error) {
	if err := s.authorize(actor); err != nil {
		return Stats{}, err
	}
	return s.store.Stats(ctx, s.cfg.Now().UTC())
}

func (s *Service) Health(ctx context.Context) error {
	return s.store.Health(ctx)
}

func (s *Service) Reap(ctx context.Context, retention time.Duration) error {
	return s.store.Reap(ctx, s.cfg.Now().UTC(), retention)
}

func (s *Service) publish(action string, m Memory) {
	if s.cfg.Publish != nil {
		s.cfg.Publish(Event{ID: m.ID, Action: action, Scope: m.Scope, OriginScope: m.OriginScope,
			Key: m.Key, Kind: m.Kind, Provenance: m.Provenance})
	}
}
