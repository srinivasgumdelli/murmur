package memory

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	Store
	put          func(Memory, int) (Memory, error)
	search       func(Query, time.Time) (Result, error)
	recordSearch func(Actor, string, time.Time) error
	promote      func(int, string, time.Time, int) (Memory, error)
	expire       func(int, Actor, time.Time) (Memory, error)
	purge        func(int) error
}

func (f fakeStore) Put(_ context.Context, m Memory, cap int) (Memory, error) {
	return f.put(m, cap)
}

func (f fakeStore) Search(_ context.Context, q Query, at time.Time) (Result, error) {
	return f.search(q, at)
}

func (f fakeStore) RecordSearch(_ context.Context, actor Actor, sessionID string, at time.Time) error {
	return f.recordSearch(actor, sessionID, at)
}

func (f fakeStore) Promote(_ context.Context, id int, by string, at time.Time, cap int) (Memory, error) {
	return f.promote(id, by, at, cap)
}

func (f fakeStore) Expire(_ context.Context, id int, actor Actor, at time.Time) (Memory, error) {
	return f.expire(id, actor, at)
}

func (f fakeStore) Purge(_ context.Context, id int) error {
	return f.purge(id)
}

func TestServicePut(t *testing.T) {
	now := time.Date(2026, time.November, 26, 12, 0, 0, 0, time.UTC)
	actor := Actor{Agent: "worker"}
	var published []Event
	store := fakeStore{put: func(m Memory, cap int) (Memory, error) {
		if cap != 7 || m.Author != actor.Agent || m.Kind != KindNote ||
			m.Provenance != Reported || !m.CreatedAt.Equal(now) ||
			m.ExpiresAt == nil || !m.ExpiresAt.Equal(now.AddDate(0, 0, 90)) {
			t.Fatalf("incorrect server-owned values: %+v, cap=%d", m, cap)
		}
		m.ID = 12
		return m, nil
	}}
	service := NewService(store, Config{
		AuthMode: "required", ScopeCap: 7, Now: func() time.Time { return now },
		Publish: func(event Event) { published = append(published, event) },
	})
	stored, err := service.Put(t.Context(), actor, PutRequest{
		Scope: "workspace:api", Key: "cmd.test", Body: "Run make test",
	})
	if err != nil || stored.ID != 12 {
		t.Fatalf("put = %+v, err = %v", stored, err)
	}
	want := []Event{{ID: 12, Scope: "workspace:api", Key: "cmd.test", Kind: KindNote, Provenance: Reported}}
	if !reflect.DeepEqual(published, want) {
		t.Fatalf("published = %+v, want %+v", published, want)
	}
}

func TestServicePutValidation(t *testing.T) {
	actor := Actor{Agent: "worker"}
	ctx := t.Context()
	service := NewService(fakeStore{}, Config{AuthMode: "required"})
	explicitZero := 0
	for _, tt := range []struct {
		name string
		req  PutRequest
		want error
	}{
		{"scope format", PutRequest{Scope: "workspace:", Body: "a"}, ErrInvalid},
		{"direct shared write", PutRequest{Scope: SharedScope, Body: "a"}, ErrForbidden},
		{"key format", PutRequest{Scope: "workspace:api", Key: "a b", Body: "a"}, ErrInvalid},
		{"unknown kind", PutRequest{Scope: "workspace:api", Body: "a", Kind: KindAny}, ErrInvalid},
		{"body limit", PutRequest{Scope: "workspace:api", Body: strings.Repeat("é", 2049)}, ErrInvalid},
		{"invalid text", PutRequest{Scope: "workspace:api", Body: "a\x00b"}, ErrInvalid},
		{"keyed none", PutRequest{Scope: "workspace:api", Key: "a", Kind: KindNone}, ErrInvalid},
		{"invalid tag", PutRequest{Scope: "workspace:api", Body: "a", Tags: []string{""}}, ErrInvalid},
		{"invalid ttl", PutRequest{Scope: "workspace:api", Body: "a", TTLDays: &explicitZero}, ErrInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := service.Put(ctx, actor, tt.req); !errors.Is(err, tt.want) {
				t.Errorf("put error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestServiceAccess(t *testing.T) {
	ctx := t.Context()
	worker := Actor{Agent: "worker"}
	lead := Actor{Agent: "lead", Lead: true}
	admin := Actor{Admin: true}
	service := NewService(fakeStore{}, Config{AuthMode: "required"})
	for _, mode := range []string{"off", "optional"} {
		svc := NewService(fakeStore{}, Config{AuthMode: mode})
		if _, err := svc.Search(ctx, worker, Query{}); !errors.Is(err, ErrAuthRequired) {
			t.Errorf("auth mode %s: %v", mode, err)
		}
	}
	if _, err := service.Get(ctx, Actor{}, 1, false); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("anonymous get: %v", err)
	}
	if _, err := service.Get(ctx, Actor{Agent: AdminAuthor}, 1, false); !errors.Is(err, ErrForbidden) {
		t.Errorf("reserved memory author get: %v", err)
	}
	if _, err := service.Promote(ctx, worker, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("worker promotion: %v", err)
	}
	if err := service.Purge(ctx, lead, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("lead purge: %v", err)
	}

	var published []Event
	service = NewService(fakeStore{
		promote: func(id int, by string, _ time.Time, _ int) (Memory, error) {
			if id != 1 || (by != "lead" && by != admin.Name()) {
				t.Fatalf("promote via store got id %d and identity %q", id, by)
			}
			return Memory{ID: 2, Provenance: Promoted, PromotedBy: by}, nil
		},
		expire: func(id int, actor Actor, _ time.Time) (Memory, error) {
			m := Memory{ID: id, Author: worker.Agent, Provenance: Promoted}
			if !CanExpire(m, actor) {
				return Memory{}, ErrForbidden
			}
			return m, nil
		},
		purge: func(id int) error {
			if id != 2 {
				t.Errorf("purge id %d, want 2", id)
			}
			return nil
		},
	}, Config{AuthMode: "required", Publish: func(event Event) { published = append(published, event) }})
	for _, actor := range []Actor{lead, admin} {
		if _, err := service.Promote(ctx, actor, 1); err != nil {
			t.Errorf("%s promotion: %v", actor.Name(), err)
		}
	}
	if err := service.Expire(ctx, worker, 2); !errors.Is(err, ErrForbidden) {
		t.Errorf("author expiring promoted entry: %v", err)
	}
	if err := service.Expire(ctx, lead, 2); err != nil {
		t.Errorf("lead expiry: %v", err)
	}
	if err := service.Purge(ctx, admin, 2); err != nil {
		t.Errorf("admin purge: %v", err)
	}
	if len(published) != 3 {
		t.Errorf("expected 3 events, got %+v", published)
	}
}

func TestServiceSearchResolvesCurrentAuthor(t *testing.T) {
	ctx := t.Context()
	actor := Actor{Agent: "worker"}
	recorded := false
	service := NewService(fakeStore{
		search: func(q Query, _ time.Time) (Result, error) {
			if q.Author != actor.Agent || q.Limit != 20 || q.SessionID != "" {
				t.Fatalf("unexpected search query %+v", q)
			}
			return Result{}, nil
		},
		recordSearch: func(a Actor, sessionID string, _ time.Time) error {
			if a != actor || sessionID != "session-a" {
				t.Fatalf("unexpected search observation: %+v, session %q", a, sessionID)
			}
			recorded = true
			return nil
		},
	}, Config{AuthMode: "required"})
	result, err := service.Search(ctx, actor, Query{Author: "me", SearchSessionID: "session-a"})
	if err != nil || result.Items == nil || !recorded {
		t.Fatalf("search = %+v, err = %v, recorded = %t", result, err, recorded)
	}
	if _, err := service.Search(ctx, actor, Query{Limit: 101}); !errors.Is(err, ErrInvalid) {
		t.Errorf("over-limit error = %v, want invalid", err)
	}
}
