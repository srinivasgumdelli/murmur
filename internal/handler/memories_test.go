package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/memory"
)

type memoryRouteStore struct {
	memory.Store
	put    func(memory.Memory) (memory.Memory, error)
	search func(memory.Query) (memory.Result, error)
}

func (s memoryRouteStore) Put(_ context.Context, entry memory.Memory, _ int) (memory.Memory, error) {
	return s.put(entry)
}

func (s memoryRouteStore) Search(_ context.Context, query memory.Query, _ time.Time) (memory.Result, error) {
	return s.search(query)
}

func (memoryRouteStore) RecordSearch(context.Context, memory.Actor, string, time.Time) error {
	return nil
}

func TestMemoryHTTP(t *testing.T) {
	store := memoryRouteStore{
		put: func(entry memory.Memory) (memory.Memory, error) {
			if entry.Author != "tester" || entry.Provenance != memory.Reported {
				t.Errorf("wrong server-managed fields: %+v", entry)
			}
			entry.ID = 1
			return entry, nil
		},
		search: func(query memory.Query) (memory.Result, error) {
			if strings.Join(query.Scopes, ",") != "workspace:repo,task:1,shared" ||
				query.Kind != memory.KindAny || query.Author != "tester" || !query.IncludeHistory ||
				query.Limit != 1 || query.SessionID != "session" || query.Since.IsZero() {
				t.Errorf("incorrect query: %+v", query)
			}
			return memory.Result{Items: []memory.Memory{{ID: 1}}, Total: 3}, nil
		},
	}
	svc := memory.NewService(store, memory.Config{AuthMode: "required"})
	mux := http.NewServeMux()
	InstallMemoryRoutes(mux, svc, "required")
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/memories", `{"scope":"workspace:repo","body":"run make test","author":"forged","provenance":"promoted"}`, 201},
		{"POST", "/memories", `{"scope":"shared","body":"untrusted"}`, 403},
		{"POST", "/memories", `{"scope":"workspace:repo","body":"first"} {}`, 400},
		{"GET", "/memories?scope=workspace:repo,task:1&scope=shared&author=me&kind=any&include_history=true&limit=1&session_id=session&since=2026-11-26T00:00:00Z", "", 200},
		{"GET", "/memories?since=invalid", "", 400},
		{"GET", "/memories/-1", "", 400},
		{"POST", "/memories/1/promote", "", 403},
		{"POST", "/memories/1/purge", "", 403},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req = req.WithContext(context.WithValue(req.Context(), principalKey{}, principal{agent: "tester"}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s %s status = %d, want %d. %s", tc.method, tc.path, rec.Code, tc.status, rec.Body.String())
		}
		if tc.status == 200 {
			var result memory.Result
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Total != 3 || len(result.Items) != 1 {
				t.Fatalf("incorrect search response: %+v (%v)", result, err)
			}
		}
	}
}

func TestMemoryRoutesRequireRequiredMode(t *testing.T) {
	for _, mode := range []string{"off", "optional"} {
		mux := http.NewServeMux()
		InstallMemoryRoutes(mux, nil, mode)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/memories", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s mode status = %d", mode, rec.Code)
		}
	}
}

func TestMemoryStreamFiltersScopes(t *testing.T) {
	n := NewNotifier()
	handler := MemoryStream(n, "required", nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{agent: "tester"})))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?types=memory&scope=workspace:repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	n.PublishMemory(memory.Event{ID: 7, Scope: "workspace:other"})
	n.PublishMemory(memory.Event{ID: 8, Scope: "workspace:repo"})
	var data [256]byte
	num, err := res.Body.Read(data[:])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data[:num]); !strings.HasPrefix(got, "event: memory\ndata: ") || !strings.Contains(got, `"id":8`) {
		t.Fatalf("wrong event: %q", got)
	}
}
