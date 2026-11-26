package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
			if entry.Kind == memory.KindNone && (entry.Body != "" || entry.SessionID != "current-session") {
				t.Errorf("wrong MCP none entry: %+v", entry)
			}
			entry.ID = 1
			return entry, nil
		},
		search: func(q memory.Query) (memory.Result, error) {
			if len(q.Scopes) != 2 || q.Scopes[0] != "workspace:repo" || q.Scopes[1] != "shared" ||
				q.Text != "test" || q.Limit != 2 || q.Author != "alice" || q.Source != "manual" ||
				q.SessionID != "entry-session" || q.SearchSessionID != "metrics-session" || !q.IncludeHistory ||
				!q.Since.Equal(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)) {
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
	info := alice.InitializeResult().ServerInfo
	if info.Name != "murmur" || info.Version == "" {
		t.Fatalf("wrong MCP server info: %+v", info)
	}
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
		wantText  string
	}{
		{alice, "memory_put", map[string]any{"scope": "workspace:repo", "body": "make test"}, false, ""},
		{alice, "memory_put", map[string]any{"scope": "workspace:repo", "kind": "none", "session_id": "current-session"}, false, ""},
		{alice, "memory_put", map[string]any{"scope": "shared", "body": "fake project fact"}, true, ""},
		{alice, "memory_put", map[string]any{"scope": "workspace:repo"}, true, ""},
		{alice, "memory_put", map[string]any{"scope": "workspace:repo", "body": strings.Repeat("é", 2049)}, true, "4096"},
		{alice, "memory_search", map[string]any{
			"q": "test", "scope": "workspace:repo,shared", "limit": 2, "author": "me", "source": "manual",
			"session_id": "entry-session", "search_session_id": "metrics-session", "include_history": true,
			"since": "2026-11-26T00:00:00Z",
		}, false, ""},
		{alice, "memory_search", map[string]any{"scope": "invalid"}, true, ""},
		{alice, "memory_search", map[string]any{"since": "yesterday"}, true, ""},
		{alice, "memory_expire", map[string]any{"id": 1}, false, ""},
		{alice, "memory_expire", map[string]any{"id": 2}, true, ""},
		{bob, "memory_expire", map[string]any{"id": 1}, true, ""},
	} {
		result, err := tc.session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil || result.IsError != tc.wantError {
			t.Errorf("%s: got %+v, %v, want tool error %v", tc.tool, result, err, tc.wantError)
			continue
		}
		if tc.wantText != "" {
			if len(result.Content) == 0 {
				t.Errorf("%s: missing error content", tc.tool)
				continue
			}
			if text, ok := result.Content[0].(*mcp.TextContent); !ok || !strings.Contains(text.Text, tc.wantText) {
				t.Errorf("%s: got error content %+v, want %q", tc.tool, result.Content[0], tc.wantText)
			}
		}
	}

	rec := httptest.NewRecorder()
	MemoryMCP(nil, "off").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled MCP returned %d", rec.Code)
	}
}
