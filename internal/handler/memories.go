package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/memory"
)

func memoryActor(r *http.Request) memory.Actor {
	return memoryActorFromContext(r.Context())
}

func memoryActorFromContext(ctx context.Context) memory.Actor {
	p := principalFromContext(ctx)
	return memory.Actor{Agent: p.agent, Lead: p.lead, Admin: p.admin}
}

func InstallMemoryRoutes(mux *http.ServeMux, svc *memory.Service, authMode string) {
	register := func(pattern string, handle func(http.ResponseWriter, *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if authMode != "required" {
				http.Error(w, "memory requires MURMUR_AUTH=required", http.StatusServiceUnavailable)
				return
			}
			if err := handle(w, r); err != nil {
				memoryError(w, err)
			}
		})
	}

	register("POST /memories", func(w http.ResponseWriter, r *http.Request) error {
		var req memory.PutRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if err := decoder.Decode(&req); err != nil {
			return memoryDecodeError(err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return memoryDecodeError(err)
		}
		entry, err := svc.Put(r.Context(), memoryActor(r), req)
		return memoryJSON(w, http.StatusCreated, entry, err)
	})
	register("GET /memories", func(w http.ResponseWriter, r *http.Request) error {
		q, err := parseMemoryQuery(r.URL.Query())
		if err != nil {
			return err
		}
		q.SearchSessionID = r.Header.Get("X-Murmur-Session-ID")
		result, err := svc.Search(r.Context(), memoryActor(r), q)
		return memoryJSON(w, http.StatusOK, result, err)
	})
	register("GET /memories/stats", func(w http.ResponseWriter, r *http.Request) error {
		stats, err := svc.Stats(r.Context(), memoryActor(r))
		return memoryJSON(w, http.StatusOK, stats, err)
	})
	register("GET /memories/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := memoryID(r)
		if err != nil {
			return err
		}
		history, err := queryBool(r.URL.Query(), "include_history")
		if err != nil {
			return err
		}
		entries, err := svc.Get(r.Context(), memoryActor(r), id, history)
		if err != nil {
			return err
		}
		if history {
			return memoryJSON(w, http.StatusOK, struct {
				memory.Memory
				History []memory.Memory `json:"history"`
			}{entries[0], entries[1:]}, nil)
		}
		return memoryJSON(w, http.StatusOK, entries[0], nil)
	})
	register("POST /memories/{id}/promote", func(w http.ResponseWriter, r *http.Request) error {
		id, err := memoryID(r)
		if err != nil {
			return err
		}
		entry, err := svc.Promote(r.Context(), memoryActor(r), id)
		return memoryJSON(w, http.StatusCreated, entry, err)
	})
	register("DELETE /memories/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id, err := memoryID(r)
		if err != nil {
			return err
		}
		return memoryJSON(w, http.StatusNoContent, nil, svc.Expire(r.Context(), memoryActor(r), id))
	})
	register("POST /memories/{id}/purge", func(w http.ResponseWriter, r *http.Request) error {
		id, err := memoryID(r)
		if err != nil {
			return err
		}
		return memoryJSON(w, http.StatusNoContent, nil, svc.Purge(r.Context(), memoryActor(r), id))
	})
}

func memoryID(r *http.Request) (int, error) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		return 0, memory.ErrInvalid
	}
	return id, nil
}

func parseMemoryQuery(values url.Values) (q memory.Query, err error) {
	q.Scopes = splitQuery(values["scope"])
	q.Tags = values["tags"]
	q.Text = values.Get("q")
	q.Author = values.Get("author")
	q.Kind = memory.Kind(values.Get("kind"))
	q.Source = memory.Source(values.Get("source"))
	q.SessionID = values.Get("session_id")
	q.IncludeHistory, err = queryBool(values, "include_history")
	if err != nil {
		return q, err
	}
	if raw := values.Get("since"); raw != "" {
		q.Since, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return q, memory.ErrInvalid
		}
	}
	if raw := values.Get("limit"); raw != "" {
		q.Limit, err = strconv.Atoi(raw)
		if err != nil || q.Limit < 1 {
			return q, memory.ErrInvalid
		}
	}
	return q, nil
}

func queryBool(values url.Values, name string) (bool, error) {
	raw := values.Get(name)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, memory.ErrInvalid
	}
	return value, nil
}

func splitQuery(values []string) []string {
	var out []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func memoryJSON(w http.ResponseWriter, status int, body any, err error) error {
	if err != nil {
		return err
	}
	if body != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
	return nil
}

func memoryDecodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return memory.ErrBodyTooLarge
	}
	return memory.ErrInvalid
}

func memoryError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, memory.ErrBodyTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, memory.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, memory.ErrAuthRequired):
		status = http.StatusServiceUnavailable
	case errors.Is(err, memory.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, memory.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, memory.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, memory.ErrScopeFull):
		status = http.StatusTooManyRequests
	}
	if status == http.StatusInternalServerError {
		log.Printf("memory: %v", err)
		http.Error(w, "internal error", status)
		return
	}
	http.Error(w, err.Error(), status)
}
