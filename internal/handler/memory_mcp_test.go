package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srinivasgumdelli/murmur/internal/memory"
)

type memoryMCPStore struct {
	memoryRouteStore
}

func (s memoryMCPStore) Expire(_ context.Context, id int, actor memory.Actor, _ time.Time) (memory.Memory, error) {
	entry := memory.Memory{ID: id, Author: "alice", Provenance: memory.Reported}
	if id == 2 {
		entry.Provenance = memory.Promoted
	}
	if !memory.CanExpire(entry, actor) {
		return memory.Memory{}, memory.ErrForbidden
	}
	return entry, nil
}

type mcpKeyTransport struct {
	agent string
}

func (tr mcpKeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Agent", tr.agent)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMemoryMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := memoryMCPStore{memoryRouteStore{
		put: func(entry memory.Memory) (memory.Memory, error) {
			if entry.Author != "alice" || entry.Provenance != memory.Reported {
				t.Errorf("wrong MCP author or provenance: %+v", entry)
			}
			entry.ID = 1
			return entry, nil
		},
		search: func(q memory.Query) (memory.Result, error) {
			if len(q.Scopes) != 2 || q.Scopes[0] != "workspace:repo" || q.Scopes[1] != "shared" || q.Text != "test" || q.Limit != 2 {
				t.Errorf("wrong MCP query: %+v", q)
			}
			return memory.Result{Items: []memory.Memory{{ID: 1}}, Total: 1}, nil
		},
	}}
	svc := memory.NewService(store, memory.Config{AuthMode: "required"})
	handler := MemoryMCP(svc, "required")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal{agent: r.Header.Get("X-Test-Agent")}
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}))
	defer server.Close()

	sessionFor := func(agent string) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "murmur-test", Version: "1"}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: server.URL, HTTPClient: &http.Client{Transport: mcpKeyTransport{agent: agent}},
		}, nil)
	}
	if _, err := sessionFor(""); err == nil {
		t.Fatal("unauthenticated MCP connection succeeded")
	}
	alice, err := sessionFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := sessionFor("bob")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	list, err := alice.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 3 {
		t.Fatalf("list tools: %+v, %v", list, err)
	}

	for _, tc := range []struct {
		session   *mcp.ClientSession
		tool      string
		args      map[string]any
		wantError bool
	}{
		{alice, "memory_put", map[string]any{"scope": "workspace:repo", "body": "make test"}, false},
		{alice, "memory_put", map[string]any{"scope": "shared", "body": "fake project fact"}, true},
		{alice, "memory_put", map[string]any{"scope": "workspace:repo", "body": ""}, true},
		{alice, "memory_search", map[string]any{"q": "test", "scope": "workspace:repo,shared", "limit": 2}, false},
		{alice, "memory_search", map[string]any{"scope": "invalid"}, true},
		{alice, "memory_expire", map[string]any{"id": 1}, false},
		{alice, "memory_expire", map[string]any{"id": 2}, true},
		{bob, "memory_expire", map[string]any{"id": 1}, true},
	} {
		result, err := tc.session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil || result.IsError != tc.wantError {
			t.Errorf("%s with %+v: got %+v, %v, want tool error %v", tc.tool, tc.args, result, err, tc.wantError)
		}
	}

	rec := httptest.NewRecorder()
	MemoryMCP(nil, "off").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled MCP returned %d", rec.Code)
	}
}
