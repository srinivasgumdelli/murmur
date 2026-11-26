package handler

import (
	"bufio"
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
	put     func(memory.Memory) (memory.Memory, error)
	search  func(memory.Query) (memory.Result, error)
	reused  bool
	promote func(int) (memory.Memory, bool, error)
}

func (s memoryRouteStore) Put(_ context.Context, entry memory.Memory, _ int, _ memory.Actor) (memory.Memory, bool, error) {
	entry, err := s.put(entry)
	return entry, err == nil && !s.reused, err
}

func (s memoryRouteStore) Promote(_ context.Context, id int, _ string, _ time.Time, _ int) (memory.Memory, bool, error) {
	return s.promote(id)
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
				query.Kind != memory.KindAny || query.Source != memory.SourceManual || query.Author != "tester" || !query.IncludeHistory ||
				query.Limit != 1 || query.SessionID != "session" || query.Since.IsZero() ||
				strings.Join(query.Tags, "|") != "linux,arm64|test|go" {
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
		{"POST", "/memories", `{"scope":"workspace:repo","body":"` + strings.Repeat("x", 4097) + `"}`, 413},
		{"POST", "/memories", `{"scope":"workspace:repo","body":"` + strings.Repeat("x", 64<<10) + `"}`, 413},
		{"GET", "/memories?scope=workspace:repo,task:1&scope=shared&tags=linux%2Carm64&tags=test,go&author=me&kind=any&source=manual&include_history=true&limit=1&session_id=session&since=2026-11-26T00:00:00Z", "", 200},
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
		if strings.Contains(tc.body, strings.Repeat("x", 64<<10)) && !strings.Contains(rec.Body.String(), "65536 bytes") {
			t.Errorf("oversized request error: %q", rec.Body.String())
		}
		if tc.status == 200 {
			var result memory.Result
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Total != 3 || len(result.Items) != 1 {
				t.Fatalf("incorrect search response: %+v (%v)", result, err)
			}
		}
	}
}

func TestMemoryHTTPReturnsOKOnReusedWrite(t *testing.T) {
	store := memoryRouteStore{
		reused: true,
		put: func(entry memory.Memory) (memory.Memory, error) {
			entry.ID = 1
			return entry, nil
		},
		promote: func(id int) (memory.Memory, bool, error) {
			return memory.Memory{ID: id + 1}, false, nil
		},
	}
	mux := http.NewServeMux()
	InstallMemoryRoutes(mux, memory.NewService(store, memory.Config{AuthMode: "required"}), "required")
	for _, tc := range []struct{ path, body string }{
		{"/memories", `{"scope":"workspace:repo","key":"cmd.test","kind":"command","source":"capture","body":"go test"}`},
		{"/memories/1/promote", ""},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req = req.WithContext(context.WithValue(req.Context(), principalKey{}, principal{agent: "lead", lead: true}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d instead of 200: %s", tc.path, rec.Code, rec.Body.String())
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
	n.PublishMemory(memory.Event{ID: 8, Action: "promote", Scope: "shared", OriginScope: "workspace:repo"})
	reader := bufio.NewReader(res.Body)
	name, err := reader.ReadString('\n')
	if err != nil || name != "event: memory\n" {
		t.Fatalf("unexpected event name %q: %v", name, err)
	}
	data, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	body, ok := strings.CutPrefix(data, "data: ")
	var event memory.Event
	if !ok || json.Unmarshal([]byte(body), &event) != nil || event.ID != 8 || event.Action != "promote" || event.OriginScope != "workspace:repo" {
		t.Fatalf("unexpected event payload: %q", data)
	}
}
