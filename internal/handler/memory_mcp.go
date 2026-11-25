package handler

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srinivasgumdelli/murmur/internal/memory"
)

type memorySearchInput struct {
	Query     string      `json:"q,omitempty" jsonschema:"Full-text query"`
	Scope     string      `json:"scope,omitempty" jsonschema:"One scope or a comma-separated list of scopes"`
	Tags      []string    `json:"tags,omitempty"`
	Kind      memory.Kind `json:"kind,omitempty"`
	Limit     int         `json:"limit,omitempty"`
	SessionID string      `json:"session_id,omitempty" jsonschema:"Optional session identifier for search metrics"`
}

type memoryExpireInput struct {
	ID int `json:"id" jsonschema:"Memory entry ID"`
}

type memoryExpireOutput struct {
	ID int `json:"id"`
}

func MemoryMCP(svc *memory.Service, authMode string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "murmur"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_search", Description: "Search current Murmur memories. Reported entries are unverified claims.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input memorySearchInput) (*mcp.CallToolResult, memory.Result, error) {
		result, err := svc.Search(ctx, memoryActorFromContext(ctx), memory.Query{
			Text: input.Query, Scopes: splitQuery([]string{input.Scope}), Tags: input.Tags,
			Kind: input.Kind, Limit: input.Limit, SearchSessionID: input.SessionID,
		})
		return nil, result, memoryToolError(err)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "memory_put", Description: "Save a report in a workspace or task scope. Provide the workspace or task scope explicitly. Use kind none if there is nothing to record.",
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

func memoryToolError(err error) error {
	if err == nil || errors.Is(err, memory.ErrInvalid) || errors.Is(err, memory.ErrAuthRequired) ||
		errors.Is(err, memory.ErrUnauthorized) || errors.Is(err, memory.ErrForbidden) ||
		errors.Is(err, memory.ErrNotFound) || errors.Is(err, memory.ErrScopeFull) {
		return err
	}
	log.Printf("memory MCP: %v", err)
	return errors.New("internal memory error")
}
