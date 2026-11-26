package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srinivasgumdelli/murmur/internal/memory"
)

type memorySearchInput struct {
	Query           string        `json:"q,omitempty" jsonschema:"Full-text query"`
	Scope           string        `json:"scope,omitempty" jsonschema:"One scope or a comma-separated list of scopes"`
	Tags            []string      `json:"tags,omitempty"`
	Kind            memory.Kind   `json:"kind,omitempty"`
	Source          memory.Source `json:"source,omitempty" jsonschema:"Filter by entry source: manual or capture"`
	Author          string        `json:"author,omitempty" jsonschema:"Filter by author. Use me for the caller"`
	SessionID       string        `json:"session_id,omitempty" jsonschema:"Filter by the session that wrote the entry"`
	SearchSessionID string        `json:"search_session_id,omitempty" jsonschema:"Track the search under this session for statistics"`
	Since           string        `json:"since,omitempty" jsonschema:"Filter by creation time at or after this RFC 3339 timestamp"`
	IncludeHistory  bool          `json:"include_history,omitempty" jsonschema:"Include expired and superseded entries"`
	Limit           int           `json:"limit,omitempty"`
}

type memoryExpireInput struct {
	ID int `json:"id" jsonschema:"Memory entry ID"`
}

type memoryExpireOutput struct {
	ID int `json:"id"`
}

func MemoryMCP(svc *memory.Service, authMode string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "murmur", Version: murmurBuildVersion()}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_search", Description: "Search Murmur memories. Results show current entries by default. Reported entries are unverified claims.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input memorySearchInput) (*mcp.CallToolResult, memory.Result, error) {
		var since time.Time
		if input.Since != "" {
			var err error
			since, err = time.Parse(time.RFC3339Nano, input.Since)
			if err != nil {
				return nil, memory.Result{}, memory.ErrInvalid
			}
		}
		result, err := svc.Search(ctx, memoryActorFromContext(ctx), memory.Query{
			Text: input.Query, Scopes: splitQuery([]string{input.Scope}), Tags: input.Tags,
			Kind: input.Kind, Source: input.Source, Author: input.Author, SessionID: input.SessionID,
			SearchSessionID: input.SearchSessionID, Since: since, IncludeHistory: input.IncludeHistory, Limit: input.Limit,
		})
		return nil, result, memoryToolError(err)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_put", Description: "Save a report in a workspace or task scope. Provide the scope and body. If there is nothing to record, use kind none and omit the key and body.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input memory.PutRequest) (*mcp.CallToolResult, memory.Memory, error) {
		result, err := svc.Put(ctx, memoryActorFromContext(ctx), input)
		return nil, result, memoryToolError(err)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_expire", Description: "Expire a memory entry you are authorized to remove.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input memoryExpireInput) (*mcp.CallToolResult, memoryExpireOutput, error) {
		err := svc.Expire(ctx, memoryActorFromContext(ctx), input.ID)
		return nil, memoryExpireOutput{ID: input.ID}, memoryToolError(err)
	})

	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authMode != "required" {
			memoryError(w, memory.ErrAuthRequired)
			return
		}
		if p := memoryActorFromContext(r.Context()); p.Name() == "" {
			memoryError(w, memory.ErrUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		transport.ServeHTTP(w, r)
	})
}

func murmurBuildVersion() string {
	const development = "0.0.0-dev"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return development
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return development + "+" + setting.Value
		}
	}
	return development
}

func memoryToolError(err error) error {
	if err == nil || errors.Is(err, memory.ErrInvalid) || errors.Is(err, memory.ErrAuthRequired) ||
		errors.Is(err, memory.ErrUnauthorized) || errors.Is(err, memory.ErrForbidden) ||
		errors.Is(err, memory.ErrNotFound) || errors.Is(err, memory.ErrScopeFull) ||
		errors.Is(err, memory.ErrBodyTooLarge) {
		return err
	}
	log.Printf("memory MCP: %v", err)
	return errors.New("internal memory error")
}
