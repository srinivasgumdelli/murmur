package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/srinivasgumdelli/murmur/internal/model"
)

type registerAgentRequest struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Description  *string  `json:"description"`
	Capabilities []string `json:"capabilities"`
	Groups       []string `json:"groups"`
}

func Agents(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/agents")
		path = strings.TrimPrefix(path, "/")

		switch {
		case path == "" && r.Method == http.MethodPost:
			registerAgent(pool, w, r)
		case path == "" && r.Method == http.MethodGet:
			listAgents(pool, w, r)
		case strings.HasSuffix(path, "/heartbeat") && r.Method == http.MethodPost:
			agentName := strings.TrimSuffix(path, "/heartbeat")
			heartbeat(pool, w, r, agentName)
		case path != "" && !strings.Contains(path, "/") && r.Method == http.MethodDelete:
			deleteAgent(pool, w, r, path)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

func registerAgent(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request) {
	var req registerAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Role == "" {
		http.Error(w, "name and role are required", http.StatusBadRequest)
		return
	}
	if req.Capabilities == nil {
		req.Capabilities = []string{}
	}
	if req.Groups == nil {
		req.Groups = []string{}
	}

	var a model.Agent
	err := pool.QueryRow(r.Context(),
		`INSERT INTO agents (name, session_id, role, description, capabilities, groups, status, last_seen)
		 VALUES ($1, gen_random_uuid()::text, $2, $3, $4, $5, 'online', now())
		 ON CONFLICT (name) DO UPDATE SET session_id = gen_random_uuid()::text, role = $2, description = $3, capabilities = $4, groups = $5, status = 'online', last_seen = now()
		 RETURNING name, session_id, role, description, capabilities, groups, status, last_seen`,
		req.Name, req.Role, req.Description, req.Capabilities, req.Groups,
	).Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen)
	if err != nil {
		log.Printf("upsert agent: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	joinMsg := a.Name + " joined"
	if a.Description != nil && *a.Description != "" {
		joinMsg += " — " + *a.Description
	} else {
		joinMsg += " (" + a.Role + ")"
	}
	postSystemMessage(r.Context(), pool, joinMsg)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(a)
}

func listAgents(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request) {
	rows, err := pool.Query(r.Context(),
		`SELECT name, session_id, role, description, capabilities, groups, status, last_seen FROM agents ORDER BY last_seen DESC`)
	if err != nil {
		log.Printf("query agents: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var agents []model.Agent
	for rows.Next() {
		var a model.Agent
		if err := rows.Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen); err != nil {
			log.Printf("scan agent: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		agents = append(agents, a)
	}
	if agents == nil {
		agents = []model.Agent{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agents)
}

func deleteAgent(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, name string) {
	tx, err := pool.Begin(r.Context())
	if err != nil {
		log.Printf("begin tx: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	if _, err := tx.Exec(r.Context(), `DELETE FROM api_keys WHERE agent = $1`, name); err != nil {
		log.Printf("delete api_keys for agent %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var a model.Agent
	err = tx.QueryRow(r.Context(),
		`DELETE FROM agents WHERE name = $1
		 RETURNING name, session_id, role, description, capabilities, groups, status, last_seen`,
		name,
	).Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		log.Printf("commit delete agent %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	postSystemMessage(r.Context(), pool, a.Name+" left")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

type heartbeatResponse struct {
	model.Agent
	Inbox *Inbox `json:"inbox,omitempty"`
}

func heartbeat(pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, name string) {
	var a model.Agent
	err := pool.QueryRow(r.Context(),
		`UPDATE agents SET status = 'online', last_seen = now()
		 WHERE name = $1
		 RETURNING name, session_id, role, description, capabilities, groups, status, last_seen`,
		name,
	).Scan(&a.Name, &a.SessionID, &a.Role, &a.Description, &a.Capabilities, &a.Groups, &a.Status, &a.LastSeen)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	inbox := fetchInbox(r.Context(), pool, name)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(heartbeatResponse{Agent: a, Inbox: inbox})
}
